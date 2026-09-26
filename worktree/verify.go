package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/reposync/internal/vcs"
	"github.com/yasyf/reposync/registry"
)

// FetchGate admits one network fetch. Verify and Restore call it immediately
// before the fetch would start, passing the fetch: the gate runs it only while
// the transfer is allowed, under a context it cancels once the transfer stops
// being allowed, and returns an error wrapping ErrFetchDeferred when it refused
// or interrupted the fetch. cc-sync gates fetches on network policy.
type FetchGate func(ctx context.Context, fetch func(context.Context) error) error

// VerifyOptions controls what Verify may do beyond local reads and writes to
// the store's mirror and the checkout's pin refs.
type VerifyOptions struct {
	// FetchOrigin, when set, admits fetching origin/<trunk> into the checkout
	// when required commits are missing; nil never fetches. When it defers the
	// fetch, Verify reports the commits still missing; any other error fails
	// the verification.
	FetchOrigin FetchGate
}

// Verification is this host's readiness to restore a snapshot. Missing names
// what blocks it: hex object ids the checkout lacks (required commits, or the
// head commit and the trees and blobs of its tree and the staged tree, never
// their history), artifact digests the source lacks, and
// "omitted:<reason>:<path>" for WIP the capture could not carry, so an
// incomplete snapshot is never Ready.
type Verification struct {
	Ready    bool
	Missing  []string
	Checkout string
}

// Verify proves the receiver can restore snap: the registered checkout holds
// every required commit; the commits the snapshot builds on are pinned under
// refs/reposync/pins/ so gc keeps them — the head commit when there is no
// history, else every prerequisite a history bundle's own header names that no
// earlier link of the chain carries, which Requires must name or Verify fails
// with ErrUndeclaredPrerequisite; each history bundle, staged blob, and shipped
// LFS object hash-verifies and is imported into the store's mirror, and the
// staged tree is rebuilt there; exactly what Restore materializes is local and
// never lazily fetched — the head commit and every tree and blob of its tree
// and the staged tree, not their history — so a checkout lacking any of them,
// partial or damaged, is not Ready, while one lacking only history can be; and
// every file artifact is present in src. A bundle the mirror already imported,
// whose tip it still holds, is validated and pinned from the prerequisites
// recorded for that bundle, without re-reading its artifact; the receiver
// commits a held tip's mirrored objects build on stay pinned while any
// snapshot names the tip. A staged blob the mirror reads back corrupt is
// re-imported from its artifact, and one still read back corrupt, from a copy
// Verify cannot replace, fails with ErrCorruptObject.
// It is idempotent, and it rebuilds mirror state whose objects went missing.
// It returns *GitVersionError when the host's git predates 2.44.
func (s *Store) Verify(ctx context.Context, reg registry.Registry, snap Snapshot, src ArtifactSource, opts VerifyOptions) (Verification, error) {
	if err := requireGit(ctx); err != nil {
		return Verification{}, err
	}
	if err := snap.validate(); err != nil {
		return Verification{}, err
	}
	lock, err := s.lockRepo(ctx, snap.Worktree.Origin)
	if err != nil {
		return Verification{}, err
	}
	defer func() { _ = lock.Close() }()
	v, _, err := s.verifyLocked(ctx, reg, snap, src, opts)
	return v, err
}

// Release forgets snap on the receiver: its mirror refs go, and pins, bundle
// tips, and mirrored LFS objects no other verified snapshot names are dropped.
func (s *Store) Release(ctx context.Context, reg registry.Registry, snap Snapshot) error {
	lock, err := s.lockRepo(ctx, snap.Worktree.Origin)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	var checkout, common string
	if repo, ok := reg.ByOrigin(snap.Worktree.Origin); ok {
		checkout, common, err = receiverCheckout(repo.Path)
		if err != nil && !errors.Is(err, ErrRepoAbsent) {
			return err
		}
	}
	m := s.mirrorFor(snap.Worktree.Origin, checkout, common)
	l, err := m.load()
	if err != nil {
		return err
	}
	delete(l.Snapshots, snapshotKey(snap))
	if err := m.save(l); err != nil {
		return err
	}
	return m.reconcile(ctx, l)
}

func (s *Store) verifyLocked(ctx context.Context, reg registry.Registry, snap Snapshot, src ArtifactSource, opts VerifyOptions) (Verification, mirror, error) {
	repo, ok := reg.ByOrigin(snap.Worktree.Origin)
	if !ok {
		return Verification{}, mirror{}, fmt.Errorf("%w: %s", ErrRepoAbsent, snap.Worktree.Origin)
	}
	checkout, common, err := receiverCheckout(repo.Path)
	if err != nil {
		return Verification{}, mirror{}, err
	}
	v := Verification{Checkout: checkout}
	format, err := recvGit(ctx, nil, nil, "-C", checkout, "rev-parse", "--show-object-format")
	if err != nil {
		return v, mirror{}, err
	}
	if strings.TrimSpace(format) != snap.ObjectFormat {
		return v, mirror{}, fmt.Errorf("%w: checkout %s is %s, snapshot is %s", ErrObjectFormat, checkout, strings.TrimSpace(format), snap.ObjectFormat)
	}
	for _, o := range snap.Omitted {
		v.Missing = append(v.Missing, "omitted:"+string(o.Reason)+":"+o.Path)
	}
	commits, err := requiredCommits(ctx, checkout, repo.Trunk, snap.Requires, opts.FetchOrigin)
	if err != nil {
		return v, mirror{}, err
	}
	v.Missing = append(v.Missing, commits...)
	artifacts, err := absentArtifacts(ctx, src, snap)
	if err != nil {
		return v, mirror{}, err
	}
	v.Missing = append(v.Missing, artifacts...)
	if len(v.Missing) > 0 {
		return v, mirror{}, nil
	}
	m := s.mirrorFor(snap.Worktree.Origin, checkout, common)
	if err := m.ensure(ctx, snap.ObjectFormat); err != nil {
		return v, m, err
	}
	l, err := m.load()
	if err != nil {
		return v, m, err
	}
	entry, absent, err := m.importSnapshot(ctx, snap, src, l)
	if err != nil {
		return v, m, errors.Join(err, m.reconcile(ctx, l))
	}
	if len(absent) > 0 {
		v.Missing = absent
		return v, m, m.reconcile(ctx, l)
	}
	l.Snapshots[snapshotKey(snap)] = entry
	if err := m.save(l); err != nil {
		return v, m, errors.Join(err, m.reconcile(ctx, l))
	}
	if err := m.reconcile(ctx, l); err != nil {
		return v, m, err
	}
	v.Ready = true
	return v, m, nil
}

func receiverCheckout(path string) (root, common string, err error) {
	root, err = filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("%w: %s", ErrRepoAbsent, path)
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve checkout %s: %w", path, err)
	}
	_, common, err = vcs.GitDirs(root)
	if err != nil {
		return "", "", err
	}
	if common == "" {
		return "", "", fmt.Errorf("%w: %s is not a git checkout", ErrRepoAbsent, root)
	}
	if common, err = filepath.EvalSymlinks(common); err != nil {
		return "", "", fmt.Errorf("resolve common dir: %w", err)
	}
	return root, common, nil
}

func requiredCommits(ctx context.Context, checkout, trunk string, requires []string, gate FetchGate) ([]string, error) {
	checkoutArgs := []string{"-C", checkout}
	absent, err := missing(ctx, checkoutArgs, requires)
	if err != nil || len(absent) == 0 || gate == nil {
		return absent, err
	}
	refspec := "+refs/heads/" + trunk + ":refs/remotes/origin/" + trunk
	err = gate(ctx, func(ctx context.Context) error {
		_, err := recvGit(ctx, nil, nil, "-C", checkout, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "origin", refspec)
		return err
	})
	if err != nil && !errors.Is(err, ErrFetchDeferred) {
		return nil, fmt.Errorf("fetch origin %s: %w", trunk, err)
	}
	return missing(ctx, checkoutArgs, requires)
}

func absentArtifacts(ctx context.Context, src ArtifactSource, snap Snapshot) ([]string, error) {
	var refs []ArtifactRef
	for _, f := range snap.Files {
		if f.Content != nil {
			refs = append(refs, *f.Content)
		}
	}
	for _, o := range snap.LFSObjects {
		refs = append(refs, o.Artifact)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	has, err := src.Has(ctx, refs)
	if err != nil {
		return nil, fmt.Errorf("check artifacts: %w", err)
	}
	var absent []string
	for i, ok := range has {
		if !ok && !slices.Contains(absent, refs[i].Digest) {
			absent = append(absent, refs[i].Digest)
		}
	}
	return absent, nil
}

func (m mirror) importSnapshot(ctx context.Context, snap Snapshot, src ArtifactSource, l mirrorLedger) (mirrorEntry, []string, error) {
	var entry mirrorEntry
	if len(snap.History) == 0 {
		if err := m.pin(ctx, snap.Requires); err != nil {
			return entry, nil, err
		}
		entry.Pins = slices.Clone(snap.Requires)
	}
	if err := m.repair(ctx); err != nil {
		return entry, nil, err
	}
	have, err := listRefs(ctx, []string{"--git-dir=" + m.dir}, snapshotPrefix)
	if err != nil {
		return entry, nil, err
	}
	delivered := map[string]bool{}
	for i, b := range snap.History {
		entry.Tips = append(entry.Tips, b.Tip)
		entry.Bundles = append(entry.Bundles, b.Artifact.Digest)
		var external []string
		admit := func(prereqs []string) error {
			var err error
			if external, err = externalPrerequisites(b, prereqs, snap.Requires, delivered); err != nil {
				return err
			}
			if err := m.pin(ctx, external); err != nil {
				return err
			}
			entry.Pins = append(entry.Pins, external...)
			return nil
		}
		mirrored := have[tipPrefix+b.Tip] == b.Tip
		prereqs, recorded := l.Bundles[b.Artifact.Digest]
		if recorded && mirrored {
			err = admit(prereqs)
		} else if prereqs, err = m.importLink(ctx, src, b, admit); err == nil {
			l.Bundles[b.Artifact.Digest] = prereqs
			if _, known := l.Tips[b.Tip]; !known || !mirrored {
				l.Tips[b.Tip] = external
			}
		}
		if err != nil {
			return entry, nil, err
		}
		if i < len(snap.History)-1 {
			if err := m.carried(ctx, b.Tip, prereqs, delivered); err != nil {
				return entry, nil, err
			}
		}
	}
	slices.Sort(entry.Pins)
	entry.Pins = slices.Compact(entry.Pins)
	head, index := snapshotPrefix+snapshotKey(snap)+"/head", snapshotPrefix+snapshotKey(snap)+"/index"
	cached := have[head] != "" && have[index] != ""
	roots := []string{snap.Head.Commit}
	if cached {
		roots = append(roots, index)
	}
	absent, err := m.absentClosure(ctx, roots)
	if err != nil || len(absent) > 0 {
		return entry, absent, err
	}
	if !cached {
		staged, err := m.buildIndex(ctx, snap, src)
		if err != nil {
			return entry, nil, err
		}
		refs := []string{"update " + head + " " + snap.Head.Commit, "update " + index + " " + staged}
		if err := updateRefs(ctx, []string{"--git-dir=" + m.dir}, refs); err != nil {
			return entry, nil, fmt.Errorf("record snapshot refs: %w", err)
		}
	}
	if err := m.healStaged(ctx, snap, src); err != nil {
		return entry, nil, err
	}
	for _, o := range snap.LFSObjects {
		if err := m.importLFS(ctx, src, o); err != nil {
			return entry, nil, err
		}
		entry.LFS = append(entry.LFS, o.OID)
	}
	return entry, nil, nil
}

func (m mirror) absentClosure(ctx context.Context, commits []string) ([]string, error) {
	args := append([]string{"rev-list", "--objects", "--no-walk", "--missing=print", "--quiet"}, commits...)
	out, err := m.git(ctx, nil, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("check snapshot objects: %w", err)
	}
	var absent []string
	for l := range strings.Lines(out) {
		if oid, ok := strings.CutPrefix(strings.TrimSuffix(l, "\n"), "?"); ok {
			absent = append(absent, oid)
		}
	}
	return absent, nil
}

func (m mirror) pin(ctx context.Context, commits []string) error {
	lines := make([]string, len(commits))
	for i, oid := range commits {
		lines[i] = "update " + pinPrefix + oid + " " + oid
	}
	if err := updateRefs(ctx, []string{"-C", m.checkout}, lines); err != nil {
		return fmt.Errorf("pin required commits: %w", err)
	}
	return nil
}

func externalPrerequisites(b Bundle, prereqs, requires []string, delivered map[string]bool) ([]string, error) {
	var external []string
	for _, p := range prereqs {
		if delivered[p] {
			continue
		}
		if !slices.Contains(requires, p) {
			return nil, fmt.Errorf("%w: bundle %s builds on %s", ErrUndeclaredPrerequisite, b.Artifact.Digest, p)
		}
		external = append(external, p)
	}
	return external, nil
}

func (m mirror) carried(ctx context.Context, tip string, prereqs []string, into map[string]bool) error {
	out, err := m.git(ctx, nil, nil, append([]string{"rev-list", tip, "--not"}, prereqs...)...)
	if err != nil {
		return fmt.Errorf("list commits bundle tip %s carries: %w", tip, err)
	}
	for l := range strings.Lines(out) {
		into[strings.TrimSuffix(l, "\n")] = true
	}
	return nil
}

func (m mirror) importLink(ctx context.Context, src ArtifactSource, b Bundle, admit func(prereqs []string) error) ([]string, error) {
	spool, err := os.CreateTemp(m.scratch, ".bundle-*")
	if err != nil {
		return nil, fmt.Errorf("spool bundle: %w", err)
	}
	defer func() { _ = os.Remove(spool.Name()) }()
	if err := copyVerified(ctx, src, b.Artifact, spool); err != nil {
		_ = spool.Close()
		return nil, err
	}
	if err := spool.Close(); err != nil {
		return nil, fmt.Errorf("spool bundle: %w", err)
	}
	prereqs, err := bundlePrerequisites(spool.Name())
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", b.Artifact.Digest, err)
	}
	if err := admit(prereqs); err != nil {
		return nil, err
	}
	if _, err := m.git(ctx, nil, nil, "bundle", "verify", "-q", spool.Name()); err != nil {
		return nil, fmt.Errorf("verify bundle %s: %w", b.Artifact.Digest, err)
	}
	heads, err := m.git(ctx, nil, nil, "bundle", "list-heads", spool.Name())
	if err != nil {
		return nil, fmt.Errorf("list bundle heads: %w", err)
	}
	var head string
	for l := range strings.Lines(heads) {
		if oid, ref, ok := strings.Cut(strings.TrimSuffix(l, "\n"), " "); ok && oid == b.Tip {
			head = ref
		}
	}
	if head == "" {
		return nil, fmt.Errorf("%w: bundle %s does not carry tip %s", ErrArtifactMismatch, b.Artifact.Digest, b.Tip)
	}
	if _, err := m.git(ctx, nil, nil, "fetch", "-q", "--no-tags", "--no-write-fetch-head", spool.Name(), "+"+head+":"+tipPrefix+b.Tip); err != nil {
		return nil, fmt.Errorf("import bundle %s: %w", b.Artifact.Digest, err)
	}
	return prereqs, nil
}

func (m mirror) buildIndex(ctx context.Context, snap Snapshot, src ArtifactSource) (string, error) {
	var hashed []string
	for _, e := range snap.Index {
		if e.Blob != nil && !slices.Contains(hashed, e.OID) {
			if err := m.importBlob(ctx, src, e); err != nil {
				return "", err
			}
			hashed = append(hashed, e.OID)
		}
	}
	tmp, err := os.MkdirTemp(m.scratch, ".index-*")
	if err != nil {
		return "", fmt.Errorf("staged index scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := m.git(ctx, env, nil, "read-tree", snap.Head.Commit); err != nil {
		return "", err
	}
	zero := strings.Repeat("0", len(snap.Head.Commit))
	var info strings.Builder
	for _, e := range snap.Index {
		mode, oid := e.Mode, e.OID
		if mode == "" {
			mode, oid = "0", zero
		}
		info.WriteString(mode + " " + oid + "\t" + e.Path + "\x00")
	}
	if _, err := m.git(ctx, env, strings.NewReader(info.String()), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := m.git(ctx, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	stamp := snap.CapturedAt.UTC().Format(time.RFC3339)
	ident := []string{
		"GIT_AUTHOR_NAME=reposync", "GIT_AUTHOR_EMAIL=reposync@localhost", "GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_NAME=reposync", "GIT_COMMITTER_EMAIL=reposync@localhost", "GIT_COMMITTER_DATE=" + stamp,
	}
	commit, err := m.git(ctx, ident, nil, "commit-tree", "--no-gpg-sign", strings.TrimSpace(tree), "-p", snap.Head.Commit, "-m", "reposync staged index "+snap.Digest)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(commit), nil
}

func (m mirror) importBlob(ctx context.Context, src ArtifactSource, e IndexEntry) error {
	got, err := m.hashObject(ctx, func(w io.Writer) error { return copyVerified(ctx, src, *e.Blob, w) }, "-w")
	if err != nil {
		return fmt.Errorf("staged blob %s: %w", e.Path, err)
	}
	if got != e.OID {
		return fmt.Errorf("%w: staged blob %s hashes to %s, manifest says %s", ErrArtifactMismatch, e.Path, got, e.OID)
	}
	return nil
}

func (m mirror) healStaged(ctx context.Context, snap Snapshot, src ArtifactSource) error {
	var checked []string
	for _, e := range snap.Index {
		if e.Blob == nil || slices.Contains(checked, e.OID) {
			continue
		}
		checked = append(checked, e.OID)
		stored, err := m.storedBlob(ctx, e)
		if err != nil {
			return err
		}
		if stored == e.OID {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, "objects", e.OID[:2], e.OID[2:])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("drop corrupt staged blob %s: %w", e.Path, err)
		}
		if err := m.importBlob(ctx, src, e); err != nil {
			return err
		}
		if stored, err = m.storedBlob(ctx, e); err != nil {
			return err
		}
		if stored != e.OID {
			return fmt.Errorf("%w: staged blob %s %s reads back as %s", ErrCorruptObject, e.Path, e.OID, stored)
		}
	}
	return nil
}

func (m mirror) storedBlob(ctx context.Context, e IndexEntry) (string, error) {
	stored, err := m.hashObject(ctx, func(w io.Writer) error {
		return recvGitTo(ctx, nil, nil, w, "--git-dir="+m.dir, "cat-file", "blob", e.OID)
	})
	if err != nil {
		return "", fmt.Errorf("stored staged blob %s: %w", e.Path, err)
	}
	return stored, nil
}

func (m mirror) hashObject(ctx context.Context, write func(io.Writer) error, args ...string) (string, error) {
	pr, pw := io.Pipe()
	written := make(chan error, 1)
	go func() {
		err := write(pw)
		_ = pw.CloseWithError(err)
		written <- err
	}()
	out, err := m.git(ctx, nil, pr, append([]string{"hash-object", "--no-filters", "--stdin"}, args...)...)
	_ = pr.CloseWithError(io.ErrClosedPipe)
	if werr := <-written; werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
		return "", werr
	}
	return strings.TrimSpace(out), err
}

func (m mirror) importLFS(ctx context.Context, src ArtifactSource, o LFSObject) error {
	dest := m.lfsPath(o.OID)
	if ok, err := holds(dest, o.OID, o.Size); err != nil || ok {
		return err
	}
	return publishVerified(dest, func(w io.Writer) error { return copyVerified(ctx, src, o.Artifact, w) })
}
