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
	"os/exec"
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

var receiverEnv = []string{"GIT_CONFIG_PARAMETERS='core.hooksPath'='/dev/null' 'core.fsmonitor'='false'", "GIT_NO_LAZY_FETCH=1"}

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
	// FetchLFS allows fetching LFS base assets missing locally, or failing
	// hash verification there, from the LFS remote; a corrupt local object is
	// replaced. cc-sync sets it only when network policy allows bulk transfer.
	FetchLFS bool
	// ApplySparse re-applies the snapshot's sparse-checkout patterns and
	// skip-worktree exceptions in the recovery worktree with git
	// sparse-checkout, which enables extensions.worktreeConfig in the receiving
	// repository's config. Without it a sparse source restores as a full
	// checkout that Restore reports as inexact; a sparse receiving checkout's
	// own selection is never inherited either way.
	ApplySparse bool
}

// Restored describes a recovery worktree. Reused means an earlier recovery
// checkout of the same source worktree was found and left untouched; Applied
// is the snapshot digest it holds and Newer reports that snap is newer.
// LFSPending lists paths still holding LFS pointers because their objects are
// not local or fail hash verification. Sparse is the source's sparse-checkout configuration, applied
// in Path only under RestoreOptions.ApplySparse. Exact reports that the
// worktree's status and sparse-checkout match the snapshot; Differences lists
// every mismatch otherwise.
type Restored struct {
	Path        string
	Branch      string
	Head        string
	Reused      bool
	Applied     string
	Newer       bool
	LFSPending  []string
	Sparse      *Sparse
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
// worktree is returned as Reused with no file touched. A Restore that fails or
// whose ctx is cancelled removes the worktree, recovery branch, and parent
// directories it created, so a retry can reuse opts.Dest; it never removes
// anything that existed before it began. It returns *GitVersionError when the
// host's git predates 2.44.
func (s *Store) Restore(ctx context.Context, reg registry.Registry, snap Snapshot, src ArtifactSource, opts RestoreOptions) (Restored, error) {
	if !filepath.IsAbs(opts.Dest) {
		return Restored{}, fmt.Errorf("restore destination %q is not absolute", opts.Dest)
	}
	if err := requireGit(ctx); err != nil {
		return Restored{}, err
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
	parents, err := absentParents(opts.Dest)
	if err != nil {
		return Restored{}, err
	}
	ns := recoveryPrefix + strings.TrimPrefix(snap.Digest, digestPrefix)[:12] + "/"
	r, err := m.materialize(ctx, snap, src, opts, branch, ns)
	cleanup := context.WithoutCancel(ctx)
	if err := errors.Join(err, updateRefs(cleanup, []string{"-C", m.checkout}, []string{"delete " + ns + "head", "delete " + ns + "index"})); err != nil {
		return Restored{}, errors.Join(err, m.discard(cleanup, opts.Dest, branch, snap.Head.Commit, parents))
	}
	return r, nil
}

func absentParents(dest string) ([]string, error) {
	var absent []string
	for dir := filepath.Dir(dest); ; dir = filepath.Dir(dir) {
		_, err := os.Lstat(dir)
		if err == nil {
			return absent, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", dir, err)
		}
		absent = append(absent, dir)
	}
}

func (m mirror) discard(ctx context.Context, dest, branch, head string, parents []string) error {
	var errs []error
	if _, err := linkedAdminDir(dest, m.common); err == nil {
		if _, err := recvGit(ctx, nil, nil, "-C", m.checkout, "worktree", "remove", "--force", "--force", dest); err != nil {
			errs = append(errs, fmt.Errorf("remove recovery worktree: %w", err))
		}
	}
	ref := "refs/heads/" + branch
	heads, err := listRefs(ctx, []string{"-C", m.checkout}, ref)
	switch {
	case err != nil:
		errs = append(errs, err)
	case heads[ref] != "":
		if err := updateRefs(ctx, []string{"-C", m.checkout}, []string{"delete " + ref + " " + head}); err != nil {
			errs = append(errs, fmt.Errorf("delete recovery branch: %w", err))
		}
	}
	for _, dir := range parents {
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove created parent: %w", err))
			break
		}
	}
	return errors.Join(errs...)
}

func (m mirror) materialize(ctx context.Context, snap Snapshot, src ArtifactSource, opts RestoreOptions, branch, ns string) (Restored, error) {
	dest := opts.Dest
	skip := []string{"GIT_LFS_SKIP_SMUDGE=1"}
	if _, err := recvGit(ctx, skip, nil, "-C", m.checkout, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", m.dir, "+"+snapshotPrefix+snapshotKey(snap)+"/*:"+ns+"*"); err != nil {
		return Restored{}, fmt.Errorf("fetch snapshot into checkout: %w", err)
	}
	if _, err := recvGit(ctx, skip, nil, "-C", m.checkout, "worktree", "add", "-q", "--no-checkout", "-b", branch, dest, ns+"head"); err != nil {
		return Restored{}, fmt.Errorf("add recovery worktree: %w", err)
	}
	admin, err := linkedAdminDir(dest, m.common)
	if err != nil {
		return Restored{}, err
	}
	if err := disableInheritedSparse(ctx, dest, admin); err != nil {
		return Restored{}, err
	}
	if _, err := recvGit(ctx, skip, nil, "-C", dest, "read-tree", "-u", "--reset", ns+"index^{tree}"); err != nil {
		return Restored{}, fmt.Errorf("check out staged tree: %w", err)
	}
	sparse := opts.ApplySparse && snap.Sparse != nil
	if sparse {
		if err := applySparsePatterns(ctx, dest, *snap.Sparse); err != nil {
			return Restored{}, err
		}
	}
	if err := markIntentToAdd(ctx, dest, snap); err != nil {
		return Restored{}, err
	}
	if sparse {
		if err := applySparseExceptions(ctx, dest, snap.Sparse.Exceptions); err != nil {
			return Restored{}, err
		}
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
	if err := applyFlags(ctx, dest, snap.Files); err != nil {
		return Restored{}, err
	}
	if err := m.writeMarker(dest, snap); err != nil {
		return Restored{}, err
	}
	diffs, err := fidelity(ctx, dest, admin, snap, pending)
	if err != nil {
		return Restored{}, err
	}
	return Restored{
		Path:        dest,
		Branch:      branch,
		Head:        snap.Head.Commit,
		Applied:     snap.Digest,
		LFSPending:  pending,
		Sparse:      snap.Sparse,
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
	var paths []string
	for _, e := range snap.IntentToAdd {
		paths = append(paths, e.Path)
	}
	for _, f := range snap.Files {
		if f.Kind != FileDeleted {
			paths = append(paths, f.Path)
		}
	}
	for p := range strings.SplitSeq(strings.TrimSuffix(tree, "\x00"), "\x00") {
		if p != "" {
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
			k := fold(d)
			if o, ok := dirs[k]; ok && o != d {
				return fmt.Errorf("%w: directories %q and %q", ErrPathCollision, o, d)
			}
			dirs[k] = d
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

func markIntentToAdd(ctx context.Context, dest string, snap Snapshot) error {
	if len(snap.IntentToAdd) == 0 {
		return nil
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("open %s: %w", dest, err)
	}
	defer func() { _ = root.Close() }()
	var paths strings.Builder
	for _, e := range snap.IntentToAdd {
		if err := placeholder(root, filepath.FromSlash(e.Path), e.Mode); err != nil {
			return fmt.Errorf("intent-to-add placeholder %s: %w", e.Path, err)
		}
		paths.WriteString(e.Path + "\x00")
	}
	stdin := strings.NewReader(paths.String())
	if _, err := recvGit(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, stdin, "-C", dest, "add", "-N", "-f", "--sparse", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return fmt.Errorf("mark intent-to-add: %w", err)
	}
	return nil
}

func placeholder(root *os.Root, name, mode string) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	switch mode {
	case "120000":
		return root.Symlink(".", name)
	case "100755":
		return root.WriteFile(name, nil, 0o755)
	}
	return root.WriteFile(name, nil, 0o644)
}

func gitMode(f FileEntry) string {
	switch {
	case f.Kind == FileDeleted:
		return "000000"
	case f.Kind == FileSymlink:
		return "120000"
	case f.Executable:
		return "100755"
	}
	return "100644"
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
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
	valid := map[string]bool{}
	split := func() (local, pending []string, err error) {
		for p, ptr := range pointers {
			if overwritten[p] {
				continue
			}
			if !valid[ptr.OID] {
				if valid[ptr.OID], err = holds(m.checkoutLFSPath(ptr.OID), ptr.OID, ptr.Size); err != nil {
					return nil, nil, err
				}
			}
			if valid[ptr.OID] {
				local = append(local, p)
			} else {
				pending = append(pending, p)
			}
		}
		return local, pending, nil
	}
	local, pending, err := split()
	if err != nil {
		return nil, err
	}
	if fetch && len(pending) > 0 {
		for _, p := range pending {
			if err := os.Remove(m.checkoutLFSPath(pointers[p].OID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("discard corrupt lfs object for %s: %w", p, err)
			}
		}
		if _, err := recvGit(ctx, nil, nil, "-C", dest, "lfs", "fetch"); err != nil {
			return nil, fmt.Errorf("fetch lfs base assets: %w", err)
		}
		if local, pending, err = split(); err != nil {
			return nil, err
		}
	}
	slices.Sort(local)
	for chunk := range slices.Chunk(local, lfsCheckoutBatch) {
		args := []string{"-C", dest, "lfs", "checkout", "--"}
		for _, p := range chunk {
			args = append(args, lfsPattern(p))
		}
		if _, err := recvGit(ctx, nil, nil, args...); err != nil {
			return nil, fmt.Errorf("hydrate lfs files: %w", err)
		}
	}
	for _, p := range local {
		unhydrated, err := pointerOnDisk(filepath.Join(dest, p), pointers[p])
		if err != nil {
			return nil, err
		}
		if unhydrated {
			pending = append(pending, p)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

func lfsPattern(path string) string {
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`\*?[]`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func pointerOnDisk(path string, ptr lfsPointer) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("stat hydrated %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > lfsPointerMax {
		return false, nil
	}
	//nolint:gosec // G304: a tracked path inside the recovery worktree this restore created.
	b, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read hydrated %s: %w", path, err)
	}
	got, ok := parseLFSPointer(b)
	return ok && got.OID == ptr.OID, nil
}

func (m mirror) checkoutLFSPath(oid string) string {
	return filepath.Join(m.common, "lfs", "objects", oid[0:2], oid[2:4], oid)
}

func (m mirror) publishLFS(o LFSObject) error {
	dest := m.checkoutLFSPath(o.OID)
	if ok, err := holds(dest, o.OID, o.Size); err != nil || ok {
		return err
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
	staged, err := recvGit(ctx, nil, nil, "-C", dest, "ls-files", "-z", "-s", "-t")
	if err != nil {
		return nil, err
	}
	oids := map[string]string{}
	var paths []string
	for _, b := range stagedBlobs(staged) {
		if !b.skipWorktree {
			oids[b.path] = b.oid
			paths = append(paths, b.path)
		}
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

func applySparsePatterns(ctx context.Context, dest string, sp Sparse) error {
	skip := []string{"GIT_LFS_SKIP_SMUDGE=1"}
	patterns := strings.NewReader(strings.Join(sp.Patterns, "\n") + "\n")
	if _, err := recvGit(ctx, skip, patterns, "-C", dest, "sparse-checkout", "set", "--no-cone", "--stdin"); err != nil {
		return fmt.Errorf("apply sparse-checkout patterns: %w", err)
	}
	if sp.Cone {
		if _, err := recvGit(ctx, skip, nil, "-C", dest, "sparse-checkout", "reapply", "--cone"); err != nil {
			return fmt.Errorf("apply cone sparse-checkout: %w", err)
		}
	}
	return nil
}

func applySparseExceptions(ctx context.Context, dest string, exceptions []string) error {
	skip := []string{"GIT_LFS_SKIP_SMUDGE=1"}
	skipped, err := skipWorktree(ctx, dest, receiverEnv)
	if err != nil {
		return err
	}
	got, err := sparseExceptions(ctx, dest, receiverEnv, skipped)
	if err != nil {
		return err
	}
	var materialize, hide []string
	for _, p := range symmetricDifference(exceptions, got) {
		if skipped[p] {
			materialize = append(materialize, p)
		} else {
			hide = append(hide, p)
		}
	}
	if len(materialize) > 0 {
		paths := strings.Join(materialize, "\x00") + "\x00"
		if _, err := recvGit(ctx, skip, strings.NewReader(paths), "-C", dest, "update-index", "--no-skip-worktree", "-z", "--stdin"); err != nil {
			return fmt.Errorf("clear skip-worktree exceptions: %w", err)
		}
		if _, err := recvGit(ctx, skip, strings.NewReader(paths), "-C", dest, "checkout-index", "-f", "-z", "--stdin"); err != nil {
			return fmt.Errorf("materialize skip-worktree exceptions: %w", err)
		}
	}
	if len(hide) > 0 {
		if _, err := recvGit(ctx, skip, strings.NewReader(strings.Join(hide, "\x00")+"\x00"), "-C", dest, "update-index", "--skip-worktree", "-z", "--stdin"); err != nil {
			return fmt.Errorf("set skip-worktree exceptions: %w", err)
		}
		root, err := os.OpenRoot(dest)
		if err != nil {
			return fmt.Errorf("open recovery worktree: %w", err)
		}
		defer func() { _ = root.Close() }()
		for _, p := range hide {
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove skip-worktree exception %s: %w", p, err)
			}
		}
	}
	return nil
}

func symmetricDifference(a, b []string) []string {
	var diff []string
	for _, p := range a {
		if !slices.Contains(b, p) {
			diff = append(diff, p)
		}
	}
	for _, p := range b {
		if !slices.Contains(a, p) {
			diff = append(diff, p)
		}
	}
	return diff
}

func sparseEnabled(ctx context.Context, dest, key string) (bool, error) {
	out, err := recvGit(ctx, nil, nil, "-C", dest, "config", "--type=bool", "--default=false", "--get", key)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", key, err)
	}
	return strings.TrimSpace(out) == "true", nil
}

func disableInheritedSparse(ctx context.Context, dest, admin string) error {
	inherited, err := sparseEnabled(ctx, dest, "core.sparseCheckout")
	if err != nil || !inherited {
		return err
	}
	perWorktree, err := sparseEnabled(ctx, dest, "extensions.worktreeConfig")
	if err != nil {
		return err
	}
	if perWorktree {
		if _, err := recvGit(ctx, []string{"GIT_LFS_SKIP_SMUDGE=1"}, nil, "-C", dest, "sparse-checkout", "disable"); err != nil {
			return fmt.Errorf("disable inherited sparse-checkout: %w", err)
		}
		return nil
	}
	if err := os.Remove(filepath.Join(admin, "info", "sparse-checkout")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("drop inherited sparse-checkout patterns: %w", err)
	}
	return nil
}

func checkoutSparse(ctx context.Context, dest, admin string) (*Sparse, error) {
	enabled, err := sparseEnabled(ctx, dest, "core.sparseCheckout")
	if err != nil || !enabled {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(admin, "info", "sparse-checkout")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	cone, err := sparseEnabled(ctx, dest, "core.sparseCheckoutCone")
	if err != nil {
		return nil, err
	}
	return readSparse(ctx, dest, receiverEnv, admin, cone)
}

func fidelity(ctx context.Context, dest, admin string, snap Snapshot, pending []string) ([]string, error) {
	st, err := readStatus(ctx, dest, receiverEnv)
	if err != nil {
		return nil, err
	}
	zero := strings.Repeat("0", len(snap.Head.Commit))
	itaModes := map[string]string{}
	for _, e := range snap.IntentToAdd {
		itaModes[e.Path] = e.Mode
	}
	var empty string
	if len(itaModes) > 0 {
		out, err := recvGit(ctx, nil, strings.NewReader(""), "-C", dest, "hash-object", "--stdin")
		if err != nil {
			return nil, fmt.Errorf("hash empty blob: %w", err)
		}
		empty = strings.TrimSpace(out)
	}
	want := map[string]pathState{}
	for _, e := range snap.Index {
		mode, oid := e.Mode, e.OID
		switch ita, ok := itaModes[e.Path]; {
		case mode == "" && ok:
			mode, oid = ita, empty
		case mode == "":
			mode, oid = "000000", zero
		}
		ps := want[e.Path]
		ps.index = mode + " " + oid
		want[e.Path] = ps
	}
	flagged := map[string]FileEntry{}
	for _, f := range snap.Files {
		if f.AssumeUnchanged || f.SkipWorktree {
			flagged[f.Path] = f
			continue
		}
		ps := want[f.Path]
		ps.work = gitMode(f)
		if f.Untracked {
			ps.work = "untracked"
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
	var unseen []string
	for p, w := range want {
		if _, ok := got[p]; !ok && w.work == "untracked" {
			unseen = append(unseen, p)
		}
	}
	ignored, err := checkIgnored(ctx, dest, unseen)
	if err != nil {
		return nil, err
	}
	for _, p := range ignored {
		got[p] = pathState{work: "untracked"}
	}
	diffs, err := flagDiffs(ctx, dest, flagged)
	if err != nil {
		return nil, err
	}
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
	itaDiffs, err := intentToAddModeDiffs(ctx, dest, snap.IntentToAdd)
	if err != nil {
		return nil, err
	}
	diffs = append(diffs, itaDiffs...)
	sparse, err := checkoutSparse(ctx, dest, admin)
	if err != nil {
		return nil, err
	}
	switch {
	case snap.Sparse != nil && sparse == nil:
		diffs = append(diffs, fmt.Sprintf("sparse checkout expanded to full: source cone %t, patterns %q, exceptions %q", snap.Sparse.Cone, snap.Sparse.Patterns, snap.Sparse.Exceptions))
	case snap.Sparse == nil && sparse != nil:
		diffs = append(diffs, fmt.Sprintf("sparse checkout: want full, got %+v", *sparse))
	case sparse != nil && !sparse.equal(*snap.Sparse):
		diffs = append(diffs, fmt.Sprintf("sparse checkout: want %+v, got %+v", *snap.Sparse, *sparse))
	}
	slices.Sort(diffs)
	return diffs, nil
}

func intentToAddModeDiffs(ctx context.Context, dest string, want []IntentToAdd) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	out, err := recvGit(ctx, nil, nil, "-C", dest, "ls-files", "-z", "-s")
	if err != nil {
		return nil, fmt.Errorf("read index modes: %w", err)
	}
	modes := map[string]string{}
	for rec := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("ls-files -s record %q", rec)
		}
		modes[p], _, _ = strings.Cut(meta, " ")
	}
	var diffs []string
	for _, e := range want {
		if got := modes[e.Path]; got != e.Mode {
			diffs = append(diffs, fmt.Sprintf("%s: want intent-to-add index mode %s, got %q", e.Path, e.Mode, got))
		}
	}
	return diffs, nil
}

func checkIgnored(ctx context.Context, dest string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	stdin := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := recvGit(ctx, nil, stdin, "-C", dest, "check-ignore", "-z", "--stdin")
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check-ignore: %w", err)
	}
	return strings.FieldsFunc(out, func(r rune) bool { return r == 0 }), nil
}

func flagDiffs(ctx context.Context, dest string, want map[string]FileEntry) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	out, err := recvGit(ctx, nil, nil, append([]string{"-C", dest}, flaggedArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("read index flags: %w", err)
	}
	entries, err := parseFlagged(out)
	if err != nil {
		return nil, err
	}
	got := map[string]flaggedEntry{}
	for _, e := range entries {
		got[e.path] = e
	}
	var diffs []string
	for p, f := range want {
		if g := got[p]; g.assumeUnchanged != f.AssumeUnchanged || g.skipWorktree != f.SkipWorktree {
			diffs = append(diffs, fmt.Sprintf("%s: want assume-unchanged=%t skip-worktree=%t, got %t %t",
				p, f.AssumeUnchanged, f.SkipWorktree, g.assumeUnchanged, g.skipWorktree))
		}
	}
	return diffs, nil
}
