package worktree

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // G505: sha1 is git's object id format for sha1 repositories, not a security hash.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
)

var flaggedArgs = []string{"ls-files", "-v", "-s", "-z"}

type flaggedEntry struct {
	path            string
	mode            string
	oid             string
	assumeUnchanged bool
	skipWorktree    bool
}

func parseFlagged(out string) ([]flaggedEntry, error) {
	var entries []flaggedEntry
	for rec := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || len(f[0]) != 1 {
			return nil, fmt.Errorf("ls-files record %q", rec)
		}
		tag := f[0][0]
		e := flaggedEntry{
			path:            path,
			mode:            f[1],
			oid:             f[2],
			assumeUnchanged: tag >= 'a' && tag <= 'z',
			skipWorktree:    tag == 'S' || tag == 's',
		}
		if f[3] == "0" && (e.assumeUnchanged || e.skipWorktree) {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func (c *capture) hidden(ctx context.Context, status statusReport, snap *Snapshot, fileMode, sparse bool) ([]fileCandidate, error) {
	var out bytes.Buffer
	if err := c.src.run(ctx, nil, &out, flaggedArgs...); err != nil {
		return nil, err
	}
	flagged, err := parseFlagged(out.String())
	if err != nil || len(flagged) == 0 {
		return nil, err
	}
	shown := map[string]bool{}
	for _, e := range status.changed {
		if e.y != '.' {
			shown[e.path] = true
		}
	}
	empty, err := blobID(snap.ObjectFormat, strings.NewReader(""), 0)
	if err != nil {
		return nil, err
	}
	var ita map[string]bool
	if slices.ContainsFunc(flagged, func(e flaggedEntry) bool { return e.oid == empty }) {
		if ita, err = c.src.intentToAdd(ctx); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(c.wt.Root)
	if err != nil {
		return nil, fmt.Errorf("open worktree root: %w", err)
	}
	defer func() { _ = root.Close() }()
	var files []fileCandidate
	var gone []flaggedEntry
	for _, e := range flagged {
		if shown[e.path] {
			continue
		}
		state, err := submoduleState(ctx, c.wt.Root, c.src.env, e)
		if err != nil {
			return nil, err
		}
		if state != "" && state != "S..." {
			c.omitted = append(c.omitted, Omission{Path: e.path, Reason: OmitSubmodule})
		}
		if !blobModes[e.mode] {
			continue
		}
		info, err := root.Lstat(e.path)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || err == nil && info.IsDir():
			gone = append(gone, e)
			continue
		case err != nil:
			return nil, fmt.Errorf("lstat %s: %w", e.path, err)
		}
		cand := fileCandidate{path: e.path, indexMode: e.mode, indexOID: e.oid, assumeUnchanged: e.assumeUnchanged, skipWorktree: e.skipWorktree}
		if ita[e.path] {
			snap.IntentToAdd = append(snap.IntentToAdd, e.path)
			files = append(files, cand)
			continue
		}
		same, err := matchesIndex(root, e, info, snap.ObjectFormat, fileMode)
		if err != nil {
			return nil, err
		}
		if !same {
			files = append(files, cand)
		}
	}
	deleted, err := c.hiddenDeletions(ctx, gone, sparse)
	if err != nil {
		return nil, err
	}
	snap.Files = append(snap.Files, deleted...)
	return files, nil
}

func (c *capture) hiddenDeletions(ctx context.Context, gone []flaggedEntry, sparse bool) ([]FileEntry, error) {
	var outside []string
	for _, e := range gone {
		if e.skipWorktree && sparse {
			outside = append(outside, e.path)
		}
	}
	included, err := c.src.sparseIncluded(ctx, outside)
	if err != nil {
		return nil, err
	}
	var deleted []FileEntry
	for _, e := range gone {
		if e.skipWorktree && sparse && !included[e.path] {
			continue
		}
		deleted = append(deleted, FileEntry{Path: e.path, Kind: FileDeleted, AssumeUnchanged: e.assumeUnchanged, SkipWorktree: e.skipWorktree})
	}
	return deleted, nil
}

func matchesIndex(root *os.Root, e flaggedEntry, info fs.FileInfo, format string, fileMode bool) (bool, error) {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		if e.mode != "120000" {
			return false, nil
		}
		target, err := root.Readlink(e.path)
		if err != nil {
			return false, fmt.Errorf("readlink %s: %w", e.path, err)
		}
		id, err := blobID(format, strings.NewReader(target), int64(len(target)))
		return id == e.oid, err
	case mode.IsRegular():
		if e.mode == "120000" || fileMode && (mode&0o111 != 0) != (e.mode == "100755") {
			return false, nil
		}
		f, err := root.OpenFile(e.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false, fmt.Errorf("%w: open %s: %w", ErrBusy, e.path, err)
		}
		defer func() { _ = f.Close() }()
		id, err := blobID(format, f, info.Size())
		return id == e.oid, err
	}
	return false, nil
}

func blobID(format string, r io.Reader, size int64) (string, error) {
	var h hash.Hash
	switch format {
	case "sha1":
		//nolint:gosec // G401: sha1 is git's object id format for sha1 repositories, not a security hash.
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	default:
		return "", fmt.Errorf("object format %q", format)
	}
	_, _ = fmt.Fprintf(h, "blob %d\x00", size)
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("hash worktree file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func applyFlags(ctx context.Context, dest string, files []FileEntry) error {
	for _, flag := range []struct {
		arg string
		set func(FileEntry) bool
	}{
		{"--assume-unchanged", func(f FileEntry) bool { return f.AssumeUnchanged }},
		{"--skip-worktree", func(f FileEntry) bool { return f.SkipWorktree }},
	} {
		var paths []string
		for _, f := range files {
			if flag.set(f) {
				paths = append(paths, f.Path)
			}
		}
		if len(paths) == 0 {
			continue
		}
		stdin := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
		if _, err := recvGit(ctx, nil, stdin, "-C", dest, "update-index", flag.arg, "-z", "--stdin"); err != nil {
			return fmt.Errorf("re-apply %s: %w", flag.arg, err)
		}
	}
	return nil
}
