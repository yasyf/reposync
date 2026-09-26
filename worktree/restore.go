package worktree

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/daemonkit/durable"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/yasyf/reposync/registry"
)

const (
	lfsCheckoutBatch = 256
)

// RestoreOptions places a restored snapshot.
type RestoreOptions struct {
	// Dest is the absolute path of the new recovery worktree; it must not exist.
	Dest string
	// Branch names the recovery branch; "" means
	// recovery/<branch|detached>-<source>-<YYYYMMDD-HHMM>. An existing branch
	// gets a -2, -3, ... suffix.
	Branch string
	// Fresh ignores an existing recovery checkout of the same source worktree
	// and creates another; nothing is ever overwritten either way.
	Fresh bool
	// FetchLFS allows fetching LFS base assets missing locally from the LFS
	// remote; cc-sync sets it only when network policy allows bulk transfer.
	FetchLFS bool
}

// Restored describes a recovery worktree. Reused means an earlier recovery
// checkout of the same source worktree was found and left untouched; Applied
// is the snapshot digest it holds and Newer reports that snap is newer.
// LFSPending lists paths still holding LFS pointers because their objects are
// not local. Exact reports that the worktree's status matches the snapshot;
// Differences lists every mismatch otherwise.
type Restored struct {
	Path        string
	Branch      string
	Head        string
	Reused      bool
	Applied     string
	Newer       bool
	LFSPending  []string
	Exact       bool
	Differences []string
}

// NotReadyError means Verify found the snapshot unrestorable on this host.
type NotReadyError struct {
	Missing []string
}

func (e *NotReadyError) Error() string {
	return "snapshot not ready to restore: missing " + strings.Join(e.Missing, ", ")
}

type recoveryMarker struct {
	Source      string    `json:"source"`
	WorktreeID  string    `json:"worktree_id"`
	Incarnation uint64    `json:"incarnation"`
	Digest      string    `json:"digest"`
	CapturedAt  time.Time `json:"captured_at"`
}

func (r recoveryMarker) Validate() error {
	if r.Source == "" || !isHex(r.WorktreeID, 32) || !strings.HasPrefix(r.Digest, digestPrefix) {
		return fmt.Errorf("recovery marker %+v", r)
	}
	return nil
}

type pathState struct {
	index string
	work  string
}

// Restore verifies snap and materializes it as a new linked worktree of the
// registered checkout at opts.Dest on a recovery branch: history and the
// staged index exactly, shipped and locally available LFS objects hydrated,
// then every worktree file, symlink, deletion, and intent-to-add. Hooks never
// run. Unless opts.Fresh, an existing recovery checkout of the same source
// worktree is returned as Reused with no file touched.
func (s *Store) Restore(ctx context.Context, reg registry.Registry, snap Snapshot, src ArtifactSource, opts RestoreOptions) (Restored, error) {
	if !filepath.IsAbs(opts.Dest) {
		return Restored{}, fmt.Errorf("restore destination %q is not absolute", opts.Dest)
	}
	if err := snap.validate(); err != nil {
		return Restored{}, err
	}
	lock, err := s.lockRepo(ctx, snap.Worktree.Origin)
	if err != nil {
		return Restored{}, err
	}
	defer func() { _ = lock.Close() }()
	v, m, err := s.verifyLocked(ctx, reg, snap, src, VerifyOptions{})
	if err != nil {
		return Restored{}, err
	}
	if !v.Ready {
		return Restored{}, &NotReadyError{Missing: v.Missing}
	}
	if !opts.Fresh {
		r, ok, err := m.sibling(ctx, snap)
		if err != nil || ok {
			return r, err
		}
	}
	if _, err := os.Lstat(opts.Dest); err == nil {
		return Restored{}, fmt.Errorf("%w: %s", ErrDestinationExists, opts.Dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Restored{}, fmt.Errorf("stat %s: %w", opts.Dest, err)
	}
	branch, err := pickBranch(ctx, m.checkout, snap, opts.Branch)
	if err != nil {
		return Restored{}, err
	}
	if err := m.checkCollisions(ctx, snap, opts.Dest); err != nil {
		return Restored{}, err
	}
	return m.restore(ctx, snap, src, opts, branch)
}

func (m mirror) restore(ctx context.Context, snap Snapshot, src ArtifactSource, opts RestoreOptions, branch string) (Restored, error) {
	ns := recoveryPrefix + strings.TrimPrefix(snap.Digest, digestPrefix)[:12] + "/"
	skip := []string{"GIT_LFS_SKIP_SMUDGE=1"}
	if _, err := recvGit(ctx, skip, nil, "-C", m.checkout, "fetch", "-q", "--no-tags", "--no-write-fetch-head", m.dir, "+"+snapshotPrefix+snapshotKey(snap)+"/*:"+ns+"*"); err != nil {
		return Restored{}, fmt.Errorf("fetch snapshot into checkout: %w", err)
	}
	r, err := m.materialize(ctx, snap, src, opts, branch, ns)
	return r, errors.Join(err, updateRefs(ctx, []string{"-C", m.checkout}, []string{"delete " + ns + "head", "delete " + ns + "index"}))
}

func (m mirror) materialize(ctx context.Context, snap Snapshot, src ArtifactSource, opts RestoreOptions, branch, ns string) (Restored, error) {
	dest := opts.Dest
	skip := []string{"GIT_LFS_SKIP_SMUDGE=1"}
	if _, err := recvGit(ctx, skip, nil, "-C", m.checkout, "worktree", "add", "-q", "--no-checkout", "-b", branch, dest, ns+"head"); err != nil {
		return Restored{}, fmt.Errorf("add recovery worktree: %w", err)
	}
	if _, err := recvGit(ctx, skip, nil, "-C", dest, "read-tree", "-u", "--reset", ns+"index^{tree}"); err != nil {
		return Restored{}, fmt.Errorf("check out staged tree: %w", err)
	}
	overwritten := map[string]bool{}
	for _, f := range snap.Files {
		overwritten[f.Path] = true
	}
	pending, err := m.hydrateLFS(ctx, snap, dest, opts.FetchLFS, overwritten)
	if err != nil {
		return Restored{}, err
	}
	if err := applyFiles(ctx, dest, src, snap.Files); err != nil {
		return Restored{}, err
	}
	if len(snap.IntentToAdd) > 0 {
		paths := strings.NewReader(strings.Join(snap.IntentToAdd, "\x00") + "\x00")
		if _, err := recvGit(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, paths, "-C", dest, "add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return Restored{}, fmt.Errorf("mark intent-to-add: %w", err)
		}
	}
	if err := m.writeMarker(dest, snap); err != nil {
		return Restored{}, err
	}
	diffs, err := fidelity(ctx, dest, snap, pending)
	if err != nil {
		return Restored{}, err
	}
	return Restored{
		Path:        dest,
		Branch:      branch,
		Head:        snap.Head.Commit,
		Applied:     snap.Digest,
		LFSPending:  pending,
		Exact:       len(diffs) == 0,
		Differences: diffs,
	}, nil
}

func (m mirror) sibling(ctx context.Context, snap Snapshot) (Restored, bool, error) {
	listed, err := listWorktrees(ctx, m.checkout)
	if err != nil {
		return Restored{}, false, err
	}
	for i, l := range listed {
		if i == 0 || l.bare || l.prunable {
			continue
		}
		admin, err := linkedAdminDir(l.path, m.common)
		if err != nil {
			return Restored{}, false, err
		}
		mk, err := durable.ReadFile[recoveryMarker](filepath.Join(admin, "reposync", "recovery.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Restored{}, false, fmt.Errorf("read recovery marker of %s: %w", l.path, err)
		}
		if mk.Source != snap.Source || mk.WorktreeID != snap.Worktree.ID || mk.Incarnation != snap.Worktree.Incarnation {
			continue
		}
		return Restored{
			Path:    l.path,
			Branch:  l.branch,
			Head:    l.head,
			Reused:  true,
			Applied: mk.Digest,
			Newer:   mk.Digest != snap.Digest && snap.CapturedAt.After(mk.CapturedAt),
		}, true, nil
	}
	return Restored{}, false, nil
}

func (m mirror) writeMarker(dest string, snap Snapshot) error {
	admin, err := linkedAdminDir(dest, m.common)
	if err != nil {
		return err
	}
	b, err := durable.Marshal(recoveryMarker{
		Source:      snap.Source,
		WorktreeID:  snap.Worktree.ID,
		Incarnation: snap.Worktree.Incarnation,
		Digest:      snap.Digest,
		CapturedAt:  snap.CapturedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("encode recovery marker: %w", err)
	}
	dir := filepath.Join(admin, "reposync")
	if err := durable.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := durable.WriteFile(filepath.Join(dir, "recovery.json"), b, 0o600); err != nil {
		return fmt.Errorf("write recovery marker: %w", err)
	}
	return nil
}

func pickBranch(ctx context.Context, checkout string, snap Snapshot, want string) (string, error) {
	if want == "" {
		name := snap.Head.Branch
		if name == "" {
			name = "detached"
		}
		want = "recovery/" + name + "-" + snap.Source + "-" + snap.CapturedAt.UTC().Format("20060102-1504")
	}
	if _, err := recvGit(ctx, nil, nil, "-C", checkout, "check-ref-format", "--branch", want); err != nil {
		return "", fmt.Errorf("recovery branch %q: %w", want, err)
	}
	heads, err := listRefs(ctx, []string{"-C", checkout}, "refs/heads/")
	if err != nil {
		return "", err
	}
	branch := want
	for n := 2; heads["refs/heads/"+branch] != ""; n++ {
		branch = want + "-" + strconv.Itoa(n)
	}
	return branch, nil
}

func (m mirror) checkCollisions(ctx context.Context, snap Snapshot, dest string) error {
	caseFold, normFold, err := folding(dest)
	if err != nil || (!caseFold && !normFold) {
		return err
	}
	tree, err := m.git(ctx, nil, nil, "ls-tree", "-r", "-z", "--name-only", snapshotPrefix+snapshotKey(snap)+"/index")
	if err != nil {
		return err
	}
	deleted := map[string]bool{}
	var paths []string
	for _, f := range snap.Files {
		if f.Kind == FileDeleted {
			deleted[f.Path] = true
		} else {
			paths = append(paths, f.Path)
		}
	}
	for p := range strings.SplitSeq(strings.TrimSuffix(tree, "\x00"), "\x00") {
		if p != "" && !deleted[p] {
			paths = append(paths, p)
		}
	}
	folder := cases.Fold()
	fold := func(p string) string {
		if normFold {
			p = norm.NFC.String(p)
		}
		if caseFold {
			p = folder.String(p)
		}
		return p
	}
	files, dirs := map[string]string{}, map[string]string{}
	for _, p := range paths {
		k := fold(p)
		if o, ok := files[k]; ok && o != p {
			return fmt.Errorf("%w: %q and %q", ErrPathCollision, o, p)
		}
		files[k] = p
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			dirs[fold(d)] = d
		}
	}
	for k, p := range files {
		if d, ok := dirs[k]; ok && d != p {
			return fmt.Errorf("%w: file %q and directory %q", ErrPathCollision, p, d)
		}
	}
	return nil
}

func folding(dest string) (caseFold, normFold bool, err error) {
	dir := filepath.Dir(dest)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	probe, err := os.CreateTemp(dir, ".reposync-probe-é-")
	if err != nil {
		return false, false, fmt.Errorf("probe %s: %w", dir, err)
	}
	name := probe.Name()
	defer func() { _ = os.Remove(name) }()
	if err := probe.Close(); err != nil {
		return false, false, fmt.Errorf("probe %s: %w", dir, err)
	}
	info, err := os.Stat(name)
	if err != nil {
		return false, false, fmt.Errorf("probe %s: %w", dir, err)
	}
	same := func(alt string) bool {
		other, err := os.Stat(filepath.Join(dir, alt))
		return err == nil && os.SameFile(info, other)
	}
	base := filepath.Base(name)
	return same(strings.ToUpper(base)), same(norm.NFD.String(base)), nil
}

func applyFiles(ctx context.Context, dest string, src ArtifactSource, files []FileEntry) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("open %s: %w", dest, err)
	}
	defer func() { _ = root.Close() }()
	for _, deletions := range []bool{true, false} {
		for _, f := range files {
			if (f.Kind == FileDeleted) != deletions {
				continue
			}
			if err := applyFile(ctx, root, src, f); err != nil {
				return fmt.Errorf("restore %s: %w", f.Path, err)
			}
		}
	}
	return nil
}

func applyFile(ctx context.Context, root *os.Root, src ArtifactSource, f FileEntry) error {
	name := filepath.FromSlash(f.Path)
	switch f.Kind {
	case FileDeleted:
		if err := root.Remove(name); err != nil {
			return err
		}
		for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
			if root.Remove(dir) != nil {
				break
			}
		}
		return nil
	case FileSymlink:
		var target bytes.Buffer
		if err := copyVerified(ctx, src, *f.Content, &target); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		return root.Symlink(target.String(), name)
	}
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), ".reposync-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	w, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	if err := copyVerified(ctx, src, *f.Content, w); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if f.Executable {
		mode = 0o755
	}
	if err := root.Chmod(tmp, mode); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func (m mirror) hydrateLFS(ctx context.Context, snap Snapshot, dest string, fetch bool, overwritten map[string]bool) ([]string, error) {
	if snap.LFS == nil {
		return nil, nil
	}
	for _, o := range snap.LFSObjects {
		if err := m.publishLFS(o); err != nil {
			return nil, err
		}
	}
	pointers, err := lfsPointers(ctx, dest)
	if err != nil {
		return nil, err
	}
	split := func() (local, pending []string) {
		for p, ptr := range pointers {
			switch {
			case overwritten[p]:
			case sizeIs(m.checkoutLFSPath(ptr.OID), ptr.Size):
				local = append(local, p)
			default:
				pending = append(pending, p)
			}
		}
		return local, pending
	}
	local, pending := split()
	if fetch && len(pending) > 0 {
		if _, err := recvGit(ctx, nil, nil, "-C", dest, "lfs", "fetch"); err != nil {
			return nil, fmt.Errorf("fetch lfs base assets: %w", err)
		}
		local, pending = split()
	}
	slices.Sort(local)
	for chunk := range slices.Chunk(local, lfsCheckoutBatch) {
		if _, err := recvGit(ctx, nil, nil, append([]string{"-C", dest, "lfs", "checkout", "--"}, chunk...)...); err != nil {
			return nil, fmt.Errorf("hydrate lfs files: %w", err)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

func (m mirror) checkoutLFSPath(oid string) string {
	return filepath.Join(m.common, "lfs", "objects", oid[0:2], oid[2:4], oid)
}

func (m mirror) publishLFS(o LFSObject) error {
	dest := m.checkoutLFSPath(o.OID)
	if sizeIs(dest, o.Size) {
		return nil
	}
	return publishVerified(dest, func(w io.Writer) error {
		f, err := os.Open(m.lfsPath(o.OID))
		if err != nil {
			return fmt.Errorf("open mirrored lfs object %s: %w", o.OID, err)
		}
		defer func() { _ = f.Close() }()
		return verifiedCopy(w, f, ArtifactRef{Digest: digestPrefix + o.OID, Size: o.Size, Media: MediaLFSObject})
	})
}

func lfsPointers(ctx context.Context, dest string) (map[string]lfsPointer, error) {
	staged, err := recvGit(ctx, nil, nil, "-C", dest, "ls-files", "-z", "-s")
	if err != nil {
		return nil, err
	}
	oids := map[string]string{}
	var paths []string
	for rec := range strings.SplitSeq(strings.TrimSuffix(staged, "\x00"), "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || (f[0] != "100644" && f[0] != "100755") {
			continue
		}
		oids[p] = f[1]
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil, nil
	}
	attrs, err := recvGit(ctx, nil, strings.NewReader(strings.Join(paths, "\x00")+"\x00"), "-C", dest, "check-attr", "-z", "--stdin", "filter")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(attrs, "\x00"), "\x00")
	var candidates []string
	for i := 0; i+2 < len(fields); i += 3 {
		if fields[i+2] == "lfs" {
			candidates = append(candidates, fields[i])
		}
	}
	return checkoutPointers(ctx, dest, candidates, oids)
}

func checkoutPointers(ctx context.Context, dest string, paths []string, oids map[string]string) (map[string]lfsPointer, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, p := range paths {
		in.WriteString(oids[p] + "\n")
	}
	out, err := recvGit(ctx, nil, strings.NewReader(in.String()), "-C", dest, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(strings.NewReader(out))
	pointers := map[string]lfsPointer{}
	for _, p := range paths {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read blob header for %s: %w", p, err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("blob %s for %s: %q", oids[p], p, header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("blob size for %s: %w", p, err)
		}
		body := make([]byte, size+1)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("read blob for %s: %w", p, err)
		}
		if ptr, ok := parseLFSPointer(body[:size]); ok {
			pointers[p] = ptr
		}
	}
	return pointers, nil
}

func fidelity(ctx context.Context, dest string, snap Snapshot, pending []string) ([]string, error) {
	st, err := readStatus(ctx, dest, []string{"GIT_CONFIG_PARAMETERS='core.hooksPath'='/dev/null'"})
	if err != nil {
		return nil, err
	}
	zero := strings.Repeat("0", len(snap.Head.Commit))
	want := map[string]pathState{}
	for _, e := range snap.Index {
		mode, oid := e.Mode, e.OID
		if mode == "" {
			mode, oid = "000000", zero
		}
		ps := want[e.Path]
		ps.index = mode + " " + oid
		want[e.Path] = ps
	}
	for _, f := range snap.Files {
		ps := want[f.Path]
		switch {
		case f.Untracked:
			ps.work = "untracked"
		case f.Kind == FileDeleted:
			ps.work = "000000"
		case f.Kind == FileSymlink:
			ps.work = "120000"
		case f.Executable:
			ps.work = "100755"
		default:
			ps.work = "100644"
		}
		want[f.Path] = ps
	}
	got := map[string]pathState{}
	for _, e := range st.changed {
		var ps pathState
		if e.x != '.' {
			ps.index = e.modeIndex + " " + e.oidIndex
		}
		if e.y != '.' {
			ps.work = e.modeWorktree
		}
		got[e.path] = ps
	}
	for _, p := range st.untracked {
		got[p] = pathState{work: "untracked"}
	}
	var diffs []string
	for p, w := range want {
		if g := got[p]; g != w {
			diffs = append(diffs, fmt.Sprintf("%s: want %+v, got %+v", p, w, g))
		}
	}
	for p, g := range got {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected %+v", p, g))
		}
	}
	for _, p := range pending {
		diffs = append(diffs, p+": lfs object pending")
	}
	slices.Sort(diffs)
	return diffs, nil
}
