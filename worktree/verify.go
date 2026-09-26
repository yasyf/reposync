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

// VerifyOptions controls what Verify may do beyond local reads and writes to
// the store's mirror and the checkout's pin refs.
type VerifyOptions struct {
	// FetchOrigin allows fetching origin/<trunk> into the checkout when required
	// commits are missing; cc-sync sets it only when network policy allows.
	FetchOrigin bool
}

// Verification is this host's readiness to restore a snapshot. Missing names
// what blocks it: hex commit ids the checkout lacks, artifact digests the
// source lacks, and "omitted:<reason>:<path>" for WIP the capture could not
// carry, so an incomplete snapshot is never Ready.
type Verification struct {
	Ready    bool
	Missing  []string
	Checkout string
}

// Verify proves the receiver can restore snap: the registered checkout holds
// every required commit (pinned under refs/reposync/pins/ so gc keeps them),
// each history bundle, staged blob, and shipped LFS object hash-verifies and is
// imported into the store's mirror, the staged tree is rebuilt there, and
// every file artifact is present in src. It is idempotent, and it rebuilds
// mirror state whose objects went missing.
func (s *Store) Verify(ctx context.Context, reg registry.Registry, snap Snapshot, src ArtifactSource, opts VerifyOptions) (Verification, error) {
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
	entry, err := m.importSnapshot(ctx, snap, src)
	if err != nil {
		return v, m, errors.Join(err, m.reconcile(ctx, l))
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

func requiredCommits(ctx context.Context, checkout, trunk string, requires []string, fetch bool) ([]string, error) {
	checkoutArgs := []string{"-C", checkout}
	absent, err := missing(ctx, checkoutArgs, requires)
	if err != nil || len(absent) == 0 || !fetch {
		return absent, err
	}
	refspec := "+refs/heads/" + trunk + ":refs/remotes/origin/" + trunk
	if _, err := recvGit(ctx, nil, nil, "-C", checkout, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "origin", refspec); err != nil {
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

func (m mirror) importSnapshot(ctx context.Context, snap Snapshot, src ArtifactSource) (mirrorEntry, error) {
	entry := mirrorEntry{Requires: snap.Requires}
	pins := make([]string, len(snap.Requires))
	for i, oid := range snap.Requires {
		pins[i] = "update " + pinPrefix + oid + " " + oid
	}
	if err := updateRefs(ctx, []string{"-C", m.checkout}, pins); err != nil {
		return entry, fmt.Errorf("pin required commits: %w", err)
	}
	if err := m.repair(ctx); err != nil {
		return entry, err
	}
	have, err := listRefs(ctx, []string{"--git-dir=" + m.dir}, snapshotPrefix)
	if err != nil {
		return entry, err
	}
	for _, b := range snap.History {
		entry.Tips = append(entry.Tips, b.Tip)
		if have[tipPrefix+b.Tip] == b.Tip {
			continue
		}
		if err := m.importLink(ctx, src, b); err != nil {
			return entry, err
		}
	}
	head, index := snapshotPrefix+snapshotKey(snap)+"/head", snapshotPrefix+snapshotKey(snap)+"/index"
	if have[head] == "" || have[index] == "" {
		staged, err := m.buildIndex(ctx, snap, src)
		if err != nil {
			return entry, err
		}
		refs := []string{"update " + head + " " + snap.Head.Commit, "update " + index + " " + staged}
		if err := updateRefs(ctx, []string{"--git-dir=" + m.dir}, refs); err != nil {
			return entry, fmt.Errorf("record snapshot refs: %w", err)
		}
	}
	for _, o := range snap.LFSObjects {
		if err := m.importLFS(ctx, src, o); err != nil {
			return entry, err
		}
		entry.LFS = append(entry.LFS, o.OID)
	}
	return entry, nil
}

func (m mirror) importLink(ctx context.Context, src ArtifactSource, b Bundle) error {
	spool, err := os.CreateTemp(m.scratch, ".bundle-*")
	if err != nil {
		return fmt.Errorf("spool bundle: %w", err)
	}
	defer func() { _ = os.Remove(spool.Name()) }()
	if err := copyVerified(ctx, src, b.Artifact, spool); err != nil {
		_ = spool.Close()
		return err
	}
	if err := spool.Close(); err != nil {
		return fmt.Errorf("spool bundle: %w", err)
	}
	if _, err := m.git(ctx, nil, nil, "bundle", "verify", "-q", spool.Name()); err != nil {
		return fmt.Errorf("verify bundle %s: %w", b.Artifact.Digest, err)
	}
	heads, err := m.git(ctx, nil, nil, "bundle", "list-heads", spool.Name())
	if err != nil {
		return fmt.Errorf("list bundle heads: %w", err)
	}
	var head string
	for l := range strings.Lines(heads) {
		if oid, ref, ok := strings.Cut(strings.TrimSuffix(l, "\n"), " "); ok && oid == b.Tip {
			head = ref
		}
	}
	if head == "" {
		return fmt.Errorf("%w: bundle %s does not carry tip %s", ErrArtifactMismatch, b.Artifact.Digest, b.Tip)
	}
	if _, err := m.git(ctx, nil, nil, "fetch", "-q", "--no-tags", "--no-write-fetch-head", spool.Name(), "+"+head+":"+tipPrefix+b.Tip); err != nil {
		return fmt.Errorf("import bundle %s: %w", b.Artifact.Digest, err)
	}
	return nil
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
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := copyVerified(ctx, src, *e.Blob, pw)
		_ = pw.CloseWithError(err)
		copied <- err
	}()
	out, err := m.git(ctx, nil, pr, "hash-object", "-w", "--no-filters", "--stdin")
	_ = pr.CloseWithError(io.ErrClosedPipe)
	if cerr := <-copied; cerr != nil && !errors.Is(cerr, io.ErrClosedPipe) {
		return fmt.Errorf("staged blob %s: %w", e.Path, cerr)
	}
	if err != nil {
		return fmt.Errorf("staged blob %s: %w", e.Path, err)
	}
	if got := strings.TrimSpace(out); got != e.OID {
		return fmt.Errorf("%w: staged blob %s hashes to %s, manifest says %s", ErrArtifactMismatch, e.Path, got, e.OID)
	}
	return nil
}

func (m mirror) importLFS(ctx context.Context, src ArtifactSource, o LFSObject) error {
	dest := m.lfsPath(o.OID)
	if ok, err := holds(dest, o.OID, o.Size); err != nil || ok {
		return err
	}
	return publishVerified(dest, func(w io.Writer) error { return copyVerified(ctx, src, o.Artifact, w) })
}
