package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/reposync/internal/vcs"
)

const (
	pinPrefix      = "refs/reposync/pins/"
	tipPrefix      = "refs/snapshots/tips/"
	snapshotPrefix = "refs/snapshots/"
	recoveryPrefix = "refs/reposync/recovery/"
)

var (
	// ErrArtifactMismatch means an artifact's bytes do not match its ref or the
	// manifest (digest, size, or git object id).
	ErrArtifactMismatch = errors.New("artifact does not match the manifest")
	// ErrObjectFormat means the receiver checkout's object format differs from
	// the snapshot's.
	ErrObjectFormat = errors.New("object format mismatch")
	// ErrUndeclaredPrerequisite means a history bundle builds on a commit that
	// neither the manifest's Requires names nor an earlier link of its chain
	// carries.
	ErrUndeclaredPrerequisite = errors.New("bundle prerequisite undeclared by the manifest")
	// ErrCorruptObject means an object the snapshot needs reads back as bytes
	// that do not hash to its id, from a copy Verify cannot replace.
	ErrCorruptObject = errors.New("object store serves a corrupt copy")
)

type mirror struct {
	dir      string
	ledger   string
	scratch  string
	checkout string
	common   string
}

func (s *Store) mirrorFor(origin, checkout, common string) mirror {
	return mirror{dir: s.mirrorDir(origin), ledger: s.mirrorLedgerPath(origin), scratch: s.tempDir(), checkout: checkout, common: common}
}

func (m mirror) lfsPath(oid string) string {
	return filepath.Join(m.dir, "lfs", "objects", oid[0:2], oid[2:4], oid)
}

type mirrorLedger struct {
	Schema    string                 `json:"schema"`
	Snapshots map[string]mirrorEntry `json:"snapshots"`
	Tips      map[string][]string    `json:"tips"`
	Bundles   map[string][]string    `json:"bundles"`
}

type mirrorEntry struct {
	Pins    []string `json:"pins"`
	Tips    []string `json:"tips,omitempty"`
	Bundles []string `json:"bundles,omitempty"`
	LFS     []string `json:"lfs,omitempty"`
}

func (l mirrorLedger) Validate() error {
	if l.Schema != mirrorSchema {
		return fmt.Errorf("mirror ledger schema %q, want %q", l.Schema, mirrorSchema)
	}
	if l.Snapshots == nil || l.Tips == nil || l.Bundles == nil {
		return errors.New("nil snapshots, tips, or bundles")
	}
	for key := range l.Snapshots {
		wt, digest, ok := strings.Cut(key, "/")
		if !ok || !isHex(wt, 32) || !isHex(digest, 64) {
			return fmt.Errorf("snapshot key %q", key)
		}
	}
	for tip, externals := range l.Tips {
		if err := validateOIDs(append([]string{tip}, externals...)); err != nil {
			return fmt.Errorf("tip %q: %w", tip, err)
		}
	}
	for digest, prereqs := range l.Bundles {
		if sum, ok := strings.CutPrefix(digest, digestPrefix); !ok || !isHex(sum, 64) {
			return fmt.Errorf("bundle digest %q", digest)
		}
		if err := validateOIDs(prereqs); err != nil {
			return fmt.Errorf("bundle %q: %w", digest, err)
		}
	}
	return nil
}

func validateOIDs(oids []string) error {
	for _, oid := range oids {
		if !isHex(oid, 40) && !isHex(oid, 64) {
			return fmt.Errorf("object id %q", oid)
		}
	}
	return nil
}

func (l mirrorLedger) keep() (pins, tips, bundles, lfs map[string]bool) {
	pins, tips, bundles, lfs = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range l.Snapshots {
		for _, p := range e.Pins {
			pins[p] = true
		}
		for _, t := range e.Tips {
			tips[t] = true
			for _, p := range l.Tips[t] {
				pins[p] = true
			}
		}
		for _, b := range e.Bundles {
			bundles[b] = true
		}
		for _, o := range e.LFS {
			lfs[o] = true
		}
	}
	return pins, tips, bundles, lfs
}

func (m mirror) load() (mirrorLedger, error) {
	return readDurable(m.ledger, mirrorLedger{Schema: mirrorSchema, Snapshots: map[string]mirrorEntry{}, Tips: map[string][]string{}, Bundles: map[string][]string{}})
}

func (m mirror) save(l mirrorLedger) error {
	_, tips, bundles, _ := l.keep()
	maps.DeleteFunc(l.Tips, func(tip string, _ []string) bool { return !tips[tip] })
	maps.DeleteFunc(l.Bundles, func(digest string, _ []string) bool { return !bundles[digest] })
	return writeDurable(m.ledger, l)
}

func snapshotKey(s Snapshot) string {
	return s.Worktree.ID + "/" + strings.TrimPrefix(s.Digest, digestPrefix)
}

func recvGit(ctx context.Context, env []string, stdin io.Reader, args ...string) (string, error) {
	var out bytes.Buffer
	err := recvGitTo(ctx, env, stdin, &out, args...)
	return out.String(), err
}

func recvGitTo(ctx context.Context, env []string, stdin io.Reader, stdout io.Writer, args ...string) error {
	return vcs.Exec(ctx, vcs.Cmd{
		Name:   "git",
		Args:   append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...),
		Env:    append([]string{"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1"}, env...),
		Stdin:  stdin,
		Stdout: stdout,
	})
}

func (m mirror) git(ctx context.Context, env []string, stdin io.Reader, args ...string) (string, error) {
	return recvGit(ctx, env, stdin, append([]string{"--git-dir=" + m.dir}, args...)...)
}

func (m mirror) ensure(ctx context.Context, format string) error {
	return initBareAlternate(ctx, m.dir, format, filepath.Join(m.common, "objects"))
}

func missing(ctx context.Context, gitArgs []string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	in := strings.Join(names, "\n") + "\n"
	out, err := recvGit(ctx, nil, strings.NewReader(in), append(slices.Clone(gitArgs), "cat-file", "--batch-check=%(objectname)")...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("cat-file answered %d of %d names", len(lines), len(names))
	}
	var absent []string
	for i, l := range lines {
		if strings.HasSuffix(l, " missing") {
			absent = append(absent, names[i])
		}
	}
	return absent, nil
}

func listRefs(ctx context.Context, gitArgs []string, prefix string) (map[string]string, error) {
	out, err := recvGit(ctx, nil, nil, append(slices.Clone(gitArgs), "for-each-ref", "--format=%(objectname) %(refname)", prefix)...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for l := range strings.Lines(out) {
		oid, name, ok := strings.Cut(strings.TrimSuffix(l, "\n"), " ")
		if !ok {
			return nil, fmt.Errorf("malformed for-each-ref line %q", l)
		}
		refs[name] = oid
	}
	return refs, nil
}

func updateRefs(ctx context.Context, gitArgs []string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	_, err := recvGit(ctx, nil, strings.NewReader(strings.Join(lines, "\n")+"\n"), append(slices.Clone(gitArgs), "update-ref", "--stdin")...)
	return err
}

func (m mirror) repair(ctx context.Context) error {
	refs, err := listRefs(ctx, []string{"--git-dir=" + m.dir}, snapshotPrefix)
	if err != nil {
		return err
	}
	var names []string
	for _, oid := range refs {
		if !slices.Contains(names, oid+"^{tree}") {
			names = append(names, oid+"^{tree}")
		}
	}
	absent, err := missing(ctx, []string{"--git-dir=" + m.dir}, names)
	if err != nil {
		return err
	}
	var present, del []string
	for ref, oid := range refs {
		if slices.Contains(absent, oid+"^{tree}") {
			del = append(del, "delete "+ref)
		} else {
			present = append(present, ref)
		}
	}
	broken, err := m.unreachable(ctx, present)
	if err != nil {
		return err
	}
	for i := 0; broken && i < len(present); i++ {
		damaged, err := m.unreachable(ctx, present[i:i+1])
		if err != nil {
			return err
		}
		if damaged {
			del = append(del, "delete "+present[i])
		}
	}
	return updateRefs(ctx, []string{"--git-dir=" + m.dir}, del)
}

func (m mirror) unreachable(ctx context.Context, refs []string) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	args := append([]string{"rev-list", "--objects", "--missing=print"}, refs...)
	out, err := m.git(ctx, nil, nil, append(args, "--not", "--alternate-refs")...)
	if err != nil {
		return false, fmt.Errorf("check mirror objects: %w", err)
	}
	for l := range strings.Lines(out) {
		if strings.HasPrefix(l, "?") {
			return true, nil
		}
	}
	return false, nil
}

func (m mirror) reconcile(ctx context.Context, l mirrorLedger) error {
	pins, tips, _, lfs := l.keep()
	if m.checkout != "" {
		refs, err := listRefs(ctx, []string{"-C", m.checkout}, pinPrefix)
		if err != nil {
			return err
		}
		var del []string
		for ref := range refs {
			if !pins[strings.TrimPrefix(ref, pinPrefix)] {
				del = append(del, "delete "+ref)
			}
		}
		if err := updateRefs(ctx, []string{"-C", m.checkout}, del); err != nil {
			return fmt.Errorf("unpin: %w", err)
		}
	}
	if _, err := os.Stat(m.dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	refs, err := listRefs(ctx, []string{"--git-dir=" + m.dir}, snapshotPrefix)
	if err != nil {
		return err
	}
	var del []string
	for ref := range refs {
		if tip, ok := strings.CutPrefix(ref, tipPrefix); ok {
			if !tips[tip] {
				del = append(del, "delete "+ref)
			}
			continue
		}
		key := strings.TrimPrefix(ref, snapshotPrefix)
		key = key[:strings.LastIndexByte(key, '/')]
		if _, ok := l.Snapshots[key]; !ok {
			del = append(del, "delete "+ref)
		}
	}
	if err := updateRefs(ctx, []string{"--git-dir=" + m.dir}, del); err != nil {
		return fmt.Errorf("drop mirror refs: %w", err)
	}
	return m.pruneLFS(lfs)
}

func (m mirror) pruneLFS(keep map[string]bool) error {
	root, err := os.OpenRoot(filepath.Join(m.dir, "lfs", "objects"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open mirrored lfs objects: %w", err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && !keep[d.Name()] {
			return root.Remove(p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("prune mirrored lfs objects: %w", err)
	}
	return nil
}

func copyVerified(ctx context.Context, src ArtifactSource, ref ArtifactRef, w io.Writer) error {
	r, err := src.Open(ctx, ref)
	if err != nil {
		return fmt.Errorf("open artifact %s: %w", ref.Digest, err)
	}
	defer func() { _ = r.Close() }()
	return verifiedCopy(w, r, ref)
}

func verifiedCopy(w io.Writer, r io.Reader, ref ArtifactRef) error {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), r)
	if err != nil {
		return fmt.Errorf("read artifact %s: %w", ref.Digest, err)
	}
	if got := digestPrefix + hex.EncodeToString(h.Sum(nil)); n != ref.Size || got != ref.Digest {
		return fmt.Errorf("%w: %s yielded %d bytes hashing to %s, want %d", ErrArtifactMismatch, ref.Digest, n, got, ref.Size)
	}
	return nil
}

func publishVerified(dest string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".reposync-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", dest, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", dest, err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	return nil
}

func holds(path, oid string, size int64) (bool, error) {
	//nolint:gosec // G304: an oid-addressed LFS object under a store- or checkout-owned LFS directory.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open lfs object %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	err = verifiedCopy(io.Discard, f, ArtifactRef{Digest: digestPrefix + oid, Size: size, Media: MediaLFSObject})
	if errors.Is(err, ErrArtifactMismatch) {
		return false, nil
	}
	return err == nil, err
}
