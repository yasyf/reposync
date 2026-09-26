package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

func (c *capture) hidden(ctx context.Context, status statusReport, snap *Snapshot, sparse bool) ([]fileCandidate, error) {
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
		ita, err := c.intentToAdd(ctx, e.path, e.oid)
		if err != nil {
			return nil, err
		}
		if ita {
			snap.IntentToAdd = append(snap.IntentToAdd, e.path)
		}
		files = append(files, fileCandidate{
			path: e.path, hidden: !ita, indexMode: e.mode, indexOID: e.oid,
			assumeUnchanged: e.assumeUnchanged, skipWorktree: e.skipWorktree,
		})
	}
	deleted, err := c.hiddenDeletions(ctx, gone, snap, sparse)
	if err != nil {
		return nil, err
	}
	snap.Files = append(snap.Files, deleted...)
	return files, nil
}

func (c *capture) hiddenDeletions(ctx context.Context, gone []flaggedEntry, snap *Snapshot, sparse bool) ([]FileEntry, error) {
	var deleted []FileEntry
	for _, e := range gone {
		if e.skipWorktree && sparse {
			continue
		}
		ita, err := c.intentToAdd(ctx, e.path, e.oid)
		if err != nil {
			return nil, err
		}
		if ita {
			snap.IntentToAdd = append(snap.IntentToAdd, e.path)
		}
		deleted = append(deleted, FileEntry{Path: e.path, Kind: FileDeleted, AssumeUnchanged: e.assumeUnchanged, SkipWorktree: e.skipWorktree})
	}
	return deleted, nil
}

func (c *capture) intentToAdd(ctx context.Context, path, oid string) (bool, error) {
	if oid != c.blobOID(nil) {
		return false, nil
	}
	if c.ita == nil {
		ita, err := c.src.intentToAdd(ctx)
		if err != nil {
			return false, err
		}
		c.ita = ita
	}
	return c.ita[path], nil
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
