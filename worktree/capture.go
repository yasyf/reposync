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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yasyf/reposync/internal/vcs"
)

// Limits bounds the new work one Capture does. Both budgets measure progress,
// never scope: a capture that reaches one returns *PartialError, keeps what it
// already Put, and the next capture resumes from its stat cache.
type Limits struct {
	// MaxNewBytes caps the bytes of content Put that the sink did not already
	// hold. A single larger item still goes through when it is the tick's first.
	MaxNewBytes int64
	// MaxEntries caps the artifacts Put.
	MaxEntries int
	// MaxChainLinks caps the history bundle chain before it restarts from trunk.
	MaxChainLinks int
}

// DefaultLimits are the per-capture budgets cc-sync schedules with.
func DefaultLimits() Limits {
	return Limits{MaxNewBytes: 1 << 30, MaxEntries: 200_000, MaxChainLinks: 8}
}

// CaptureOptions configures Capture. Source is the capturing host's synckit
// id; zero Limits fields take DefaultLimits.
type CaptureOptions struct {
	Source string
	Limits Limits
}

var blobModes = map[string]bool{"100644": true, "100755": true, "120000": true}

// Capture snapshots wt's uncommitted and unpublished work into sink without
// writing the source repository: no index refresh, no filter, no hook, no jj
// snapshot. It returns *DeferredError mid-operation, ErrBusy on a live lock
// or a HEAD, index, or file that moved during the capture, *PartialError when
// a progress budget ran out, and *MissingLFSError when a required LFS object
// is absent locally. An unchanged worktree yields the same Digest with no Put.
func (s *Store) Capture(ctx context.Context, wt Worktree, sink ArtifactSink, opts CaptureOptions) (Snapshot, error) {
	if opts.Source == "" {
		return Snapshot{}, errors.New("capture: empty source")
	}
	lock, err := s.lockRepo(ctx, wt.Origin)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = lock.Close() }()
	c := &capture{store: s, wt: wt, sink: sink, limits: opts.Limits.withDefaults()}
	c.budget.limits = c.limits
	snap, err := c.run(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Source = opts.Source
	snap.CapturedAt = time.Now().UTC()
	if snap.Digest, err = snap.ContentDigest(); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxNewBytes == 0 {
		l.MaxNewBytes = d.MaxNewBytes
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxChainLinks == 0 {
		l.MaxChainLinks = d.MaxChainLinks
	}
	return l
}

type budget struct {
	limits    Limits
	bytes     int64
	entries   int
	exhausted PartialReason
}

func (b *budget) allow(size int64) bool {
	switch {
	case b.exhausted != "":
	case b.entries >= b.limits.MaxEntries:
		b.exhausted = PartialEntries
	case b.bytes > 0 && b.bytes+size > b.limits.MaxNewBytes:
		b.exhausted = PartialNewBytes
	default:
		return true
	}
	return false
}

func (b *budget) spend(size int64) {
	b.bytes += size
	b.entries++
}

type capture struct {
	store     *Store
	wt        Worktree
	sink      ArtifactSink
	limits    Limits
	budget    budget
	src       source
	prev      ledger
	next      ledger
	remaining []string
	missing   []LFSObjectRef
	omitted   []Omission
}

type fileCandidate struct {
	path      string
	untracked bool
	indexMode string
	indexOID  string
}

type guard struct {
	head, jjCheckout []byte
	index            fileStat
}

func (c *capture) run(ctx context.Context) (Snapshot, error) {
	if err := c.gate(); err != nil {
		return Snapshot{}, err
	}
	before, err := c.guard()
	if err != nil {
		return Snapshot{}, err
	}
	if c.prev, err = c.store.readLedger(c.wt.ID); err != nil {
		return Snapshot{}, err
	}
	c.next = newLedger(c.wt.ID)
	c.next.Chain = slices.Clone(c.prev.Chain)
	c.next.Files, c.next.Blobs = map[string]cachedFile{}, map[string]cachedBlob{}
	if c.src, err = newSource(ctx, c.wt, c.store.privateIndexPath(c.wt.ID)); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Schema: SnapshotSchema, Worktree: c.wt}
	if c.wt.Kind != KindGit {
		if snap.JJ, snap.Head.Commit, err = c.jjState(ctx); err != nil {
			return Snapshot{}, err
		}
	}
	if c.wt.Kind == KindJJWorkspace {
		if err := c.refreshPrivateIndex(ctx, snap.Head.Commit); err != nil {
			return Snapshot{}, err
		}
	}
	if snap.ObjectFormat, err = c.src.output(ctx, "rev-parse", "--show-object-format"); err != nil {
		return Snapshot{}, err
	}
	cfg, err := c.src.config(ctx, `^(filter\.lfs\.(clean|process)|lfs\.url|remote\.origin\.lfsurl)$`)
	if err != nil {
		return Snapshot{}, err
	}
	flags, err := c.src.config(ctx, `^core\.(filemode|sparsecheckout|sparsecheckoutcone)$`, "--type=bool")
	if err != nil {
		return Snapshot{}, err
	}
	var pathspec []string
	if c.wt.Kind == KindJJWorkspace {
		pathspec = []string{".", ":(exclude,top).jj"}
	}
	status, err := readStatus(ctx, c.src.dir, c.src.env, pathspec...)
	if err != nil {
		return Snapshot{}, err
	}
	if len(status.unmerged) > 0 {
		return Snapshot{}, &DeferredError{Reason: "unmerged paths"}
	}
	if err := c.head(ctx, status, &snap); err != nil {
		return Snapshot{}, err
	}
	hist, err := c.store.captureHistory(ctx, c.wt, snap.ObjectFormat, snap.Head, &c.next, c.sink, c.limits.MaxChainLinks, &c.budget)
	if err != nil {
		return Snapshot{}, err
	}
	snap.History, snap.Requires = hist.links, hist.requires
	files := c.classify(status, &snap)
	_, lfsClean := cfg["filter.lfs.clean"]
	_, lfsProcess := cfg["filter.lfs.process"]
	usesLFS := lfsClean || lfsProcess
	attrs := map[string]string{}
	var lfsRefs []LFSObjectRef
	if usesLFS {
		if attrs, err = c.lfsAttrs(ctx, snap.Head.Commit, snap.Index, files); err != nil {
			return Snapshot{}, err
		}
		if snap.Head.Ahead > 0 {
			if lfsRefs, err = c.src.historyLFS(ctx, snap.Head.Commit, snap.Head.TrunkTip, snap.Head.TrunkBase); err != nil {
				return Snapshot{}, err
			}
		}
	}
	staged, err := c.captureIndex(ctx, snap.Index, attrs)
	if err != nil {
		return Snapshot{}, err
	}
	if usesLFS {
		info, err := c.lfsInfo(ctx, cfg, append(lfsRefs, staged...))
		if err != nil {
			return Snapshot{}, err
		}
		snap.LFS = info
		if snap.LFSObjects, err = c.shipLFS(ctx, info.Objects); err != nil {
			return Snapshot{}, err
		}
	}
	fileMode := flags["core.filemode"] != "false"
	entries, err := c.captureFiles(ctx, files, attrs, fileMode)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Files = append(snap.Files, entries...)
	if err := c.recheck(ctx, before, snap.Head.Commit); err != nil {
		return Snapshot{}, err
	}
	if flags["core.sparsecheckout"] == "true" && c.wt.GitDir != "" {
		if snap.Sparse, err = readSparse(c.wt.GitDir, flags["core.sparsecheckoutcone"] == "true"); err != nil {
			return Snapshot{}, err
		}
	}
	if err := writeDurable(c.store.ledgerPath(c.wt.ID), c.next); err != nil {
		return Snapshot{}, err
	}
	if len(c.missing) > 0 {
		return Snapshot{}, &MissingLFSError{Objects: c.missing}
	}
	if len(c.remaining) > 0 {
		slices.Sort(c.remaining)
		return Snapshot{}, &PartialError{Remaining: slices.Compact(c.remaining), Reason: c.budget.exhausted}
	}
	sortSnapshot(&snap, c.omitted)
	return snap, nil
}

func (c *capture) jjDir() string {
	if c.wt.Kind == KindGit {
		return ""
	}
	return filepath.Join(c.wt.Root, ".jj")
}

func (c *capture) gate() error {
	reason, lock, err := vcs.OpState(c.wt.GitDir, c.wt.CommonDir, c.jjDir())
	switch {
	case err != nil:
		return err
	case reason == "":
		return nil
	case lock:
		return fmt.Errorf("%w: %s", ErrBusy, reason)
	}
	return &DeferredError{Reason: reason}
}

func (c *capture) guard() (guard, error) {
	var g guard
	var err error
	if c.wt.GitDir != "" {
		if g.head, err = os.ReadFile(filepath.Join(c.wt.GitDir, "HEAD")); err != nil {
			return guard{}, fmt.Errorf("read HEAD: %w", err)
		}
		if g.index, err = lstatPath(filepath.Join(c.wt.GitDir, "index")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return guard{}, err
		}
	}
	if c.wt.Kind != KindGit {
		if g.jjCheckout, err = os.ReadFile(filepath.Join(c.wt.Root, ".jj", "working_copy", "checkout")); err != nil {
			return guard{}, fmt.Errorf("read jj checkout: %w", err)
		}
	}
	return g, nil
}

func (c *capture) recheck(ctx context.Context, before guard, head string) error {
	reason, _, err := vcs.OpState(c.wt.GitDir, c.wt.CommonDir, c.jjDir())
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("%w: %s began mid-capture", ErrBusy, reason)
	}
	after, err := c.guard()
	if err != nil {
		return err
	}
	if !bytes.Equal(before.head, after.head) || before.index != after.index || !bytes.Equal(before.jjCheckout, after.jjCheckout) {
		return fmt.Errorf("%w: HEAD, index, or jj working copy moved mid-capture", ErrBusy)
	}
	if c.wt.Kind == KindJJWorkspace {
		return nil
	}
	now, err := c.src.output(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if now != head {
		return fmt.Errorf("%w: HEAD moved to %s mid-capture", ErrBusy, now)
	}
	return nil
}

func (c *capture) jjState(ctx context.Context) (*JJ, string, error) {
	conflicts, err := jjRead(ctx, c.wt.Root, "log", "--no-graph", "-r", "conflicts() & ::@ & mutable()", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(conflicts) != "" {
		return nil, "", &DeferredError{Reason: "jj conflict"}
	}
	out, err := jjRead(ctx, c.wt.Root, "log", "--no-graph", "-r", "@", "-T",
		`change_id ++ "\t" ++ commit_id ++ "\t" ++ parents.map(|c| c.commit_id()).join(",") ++ "\t" ++ description`)
	if err != nil {
		return nil, "", err
	}
	f := strings.SplitN(out, "\t", 4)
	if len(f) != 4 {
		return nil, "", fmt.Errorf("jj @ template output %q", out)
	}
	parents := strings.Split(f[2], ",")
	if len(parents) != 1 {
		return nil, "", &DeferredError{Reason: "jj merge working copy"}
	}
	bookmarks, err := jjRead(ctx, c.wt.Root, "log", "--no-graph", "-r", "@-", "-T", `local_bookmarks.map(|b| b.name()).join("\n") ++ "\n"`)
	if err != nil {
		return nil, "", err
	}
	name := c.wt.Name
	if name == "" {
		name = "default"
	}
	jj := &JJ{Workspace: name, ChangeID: f[0], WorkingCopyCommit: f[1], Description: f[3]}
	for b := range strings.FieldsSeq(bookmarks) {
		jj.Bookmarks = append(jj.Bookmarks, b)
	}
	slices.Sort(jj.Bookmarks)
	return jj, parents[0], nil
}

func (c *capture) refreshPrivateIndex(ctx context.Context, parent string) error {
	if err := c.src.run(ctx, nil, nil, "read-tree", "-m", parent); err != nil {
		return err
	}
	return c.src.run(ctx, nil, nil, "update-index", "-q", "--refresh")
}

func (c *capture) head(ctx context.Context, status statusReport, snap *Snapshot) error {
	if c.wt.Kind != KindJJWorkspace {
		if status.commit == "" {
			return fmt.Errorf("%s: HEAD is unborn", c.wt.Root)
		}
		snap.Head.Commit, snap.Head.Branch, snap.Head.Upstream = status.commit, status.branch, status.upstream
	}
	snap.Worktree.Head, snap.Worktree.Branch = snap.Head.Commit, snap.Head.Branch
	var err error
	if snap.Head.TrunkTip, err = c.src.output(ctx, "rev-parse", "--verify", "refs/remotes/origin/"+c.wt.Trunk+"^{commit}"); err != nil {
		return err
	}
	if snap.Head.TrunkBase, err = c.src.output(ctx, "merge-base", snap.Head.Commit, snap.Head.TrunkTip); err != nil {
		return err
	}
	ahead, err := c.src.output(ctx, "rev-list", "--count", snap.Head.Commit, "^"+snap.Head.TrunkTip)
	if err != nil {
		return err
	}
	if snap.Head.Ahead, err = strconv.Atoi(ahead); err != nil {
		return fmt.Errorf("rev-list count %q: %w", ahead, err)
	}
	return nil
}

func (c *capture) classify(status statusReport, snap *Snapshot) []fileCandidate {
	var files []fileCandidate
	for _, e := range status.changed {
		if e.submoduleChanged() {
			c.omitted = append(c.omitted, Omission{Path: e.path, Reason: OmitSubmodule})
			continue
		}
		if e.x != '.' && c.wt.Kind != KindJJWorkspace {
			entry := IndexEntry{Path: e.path, Mode: e.modeIndex, OID: e.oidIndex}
			if e.modeIndex == "000000" {
				entry.Mode, entry.OID = "", ""
			}
			snap.Index = append(snap.Index, entry)
		}
		switch e.y {
		case 'D':
			snap.Files = append(snap.Files, FileEntry{Path: e.path, Kind: FileDeleted})
		case 'A':
			snap.IntentToAdd = append(snap.IntentToAdd, e.path)
			files = append(files, fileCandidate{path: e.path, indexMode: e.modeIndex, indexOID: e.oidIndex})
		case 'M', 'T':
			files = append(files, fileCandidate{path: e.path, indexMode: e.modeIndex, indexOID: e.oidIndex})
		}
	}
	for _, p := range status.untracked {
		if dir, ok := strings.CutSuffix(p, "/"); ok {
			c.omitted = append(c.omitted, Omission{Path: dir, Reason: OmitNestedRepo})
			continue
		}
		files = append(files, fileCandidate{path: p, untracked: true})
	}
	return files
}

func (c *capture) lfsAttrs(ctx context.Context, head string, index []IndexEntry, files []fileCandidate) (map[string]string, error) {
	var paths []string
	for _, e := range index {
		paths = append(paths, e.Path)
	}
	for _, f := range files {
		if !f.untracked {
			paths = append(paths, f.path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	attrs := map[string]string{}
	for _, opts := range [][]string{nil, {"--cached"}, {"--source=" + head}} {
		got, err := c.src.filterAttr(ctx, paths, opts...)
		if err != nil {
			return nil, err
		}
		for p, v := range got {
			if v == lfsFilter {
				attrs[p] = lfsFilter
			}
		}
	}
	return attrs, nil
}

func (c *capture) captureIndex(ctx context.Context, index []IndexEntry, attrs map[string]string) ([]LFSObjectRef, error) {
	var cachedRefs []ArtifactRef
	var cachedOIDs []string
	for _, e := range index {
		if cached, ok := c.prev.Blobs[e.OID]; ok && blobModes[e.Mode] {
			cachedRefs = append(cachedRefs, cached.Blob)
			cachedOIDs = append(cachedOIDs, e.OID)
		}
	}
	has, err := c.sink.Has(ctx, cachedRefs)
	if err != nil {
		return nil, fmt.Errorf("sink has: %w", err)
	}
	for i, oid := range cachedOIDs {
		if has[i] {
			c.next.Blobs[oid] = c.prev.Blobs[oid]
		}
	}
	var need []string
	for _, e := range index {
		if _, ok := c.next.Blobs[e.OID]; !ok && blobModes[e.Mode] {
			need = append(need, e.OID)
		}
	}
	sizes, err := c.src.blobSizes(ctx, need)
	if err != nil {
		return nil, err
	}
	var read []string
	queued := map[string]bool{}
	for _, e := range index {
		if _, done := c.next.Blobs[e.OID]; done || !blobModes[e.Mode] || queued[e.OID] {
			continue
		}
		if !c.budget.allow(sizes[e.OID]) {
			c.postpone(e.Path)
			continue
		}
		c.budget.spend(sizes[e.OID])
		queued[e.OID] = true
		read = append(read, e.OID)
	}
	err = c.src.readBlobs(ctx, read, func(oid string, size int64, r io.Reader) error {
		var entry cachedBlob
		if size <= lfsPointerMax {
			b, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("read blob %s: %w", oid, err)
			}
			if p, ok := parseLFSPointer(b); ok {
				entry.LFS = &p
			}
			r = bytes.NewReader(b)
		}
		ref, err := c.sink.Put(ctx, MediaBlob, r)
		if err != nil {
			return fmt.Errorf("put blob %s: %w", oid, err)
		}
		if ref.Size != size {
			return fmt.Errorf("blob %s: put %d bytes, want %d", oid, ref.Size, size)
		}
		entry.Blob = ref
		c.next.Blobs[oid] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	var staged []LFSObjectRef
	for i, e := range index {
		cached, ok := c.next.Blobs[e.OID]
		if !ok || !blobModes[e.Mode] {
			continue
		}
		index[i].Blob = &cached.Blob
		if cached.LFS != nil && (cached.LFS.strict() || attrs[e.Path] == lfsFilter) {
			staged = append(staged, LFSObjectRef{Path: e.Path, OID: cached.LFS.OID, Size: cached.LFS.Size})
		}
	}
	return staged, nil
}

func (c *capture) postpone(path string) {
	c.remaining = append(c.remaining, path)
	if f, ok := c.prev.Files[path]; ok {
		c.next.Files[path] = f
	}
}

func (c *capture) lfsInfo(ctx context.Context, cfg map[string]string, refs []LFSObjectRef) (*LFSInfo, error) {
	slices.SortFunc(refs, func(a, b LFSObjectRef) int {
		return strings.Compare(a.Path+"\x00"+a.OID, b.Path+"\x00"+b.OID)
	})
	info := &LFSInfo{Objects: slices.CompactFunc(refs, func(a, b LFSObjectRef) bool { return a.Path == b.Path && a.OID == b.OID })}
	info.Remote = cmpOr(cfg["lfs.url"], cfg["remote.origin.lfsurl"])
	if info.Remote != "" {
		return info, nil
	}
	lfsconfig := filepath.Join(c.wt.Root, ".lfsconfig")
	if _, err := os.Lstat(lfsconfig); errors.Is(err, fs.ErrNotExist) {
		return info, nil
	}
	file, err := c.src.config(ctx, `^(lfs\.url|remote\.origin\.lfsurl)$`, "-f", lfsconfig)
	if err != nil {
		return nil, err
	}
	info.Remote = cmpOr(file["lfs.url"], file["remote.origin.lfsurl"])
	return info, nil
}

func cmpOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *capture) shipLFS(ctx context.Context, refs []LFSObjectRef) ([]LFSObject, error) {
	byOID := map[string][]LFSObjectRef{}
	var oids []string
	for _, r := range refs {
		if _, ok := byOID[r.OID]; !ok {
			oids = append(oids, r.OID)
		}
		byOID[r.OID] = append(byOID[r.OID], r)
	}
	slices.Sort(oids)
	artifacts := make([]ArtifactRef, len(oids))
	for i, oid := range oids {
		artifacts[i] = lfsPointer{OID: oid, Size: byOID[oid][0].Size}.artifact()
	}
	has, err := c.sink.Has(ctx, artifacts)
	if err != nil {
		return nil, fmt.Errorf("sink has: %w", err)
	}
	var objects []LFSObject
	for i, oid := range oids {
		want := artifacts[i]
		if !has[i] {
			path := filepath.Join(c.wt.CommonDir, "lfs", "objects", oid[0:2], oid[2:4], oid)
			shipped, err := c.putLFSObject(ctx, path, want, byOID[oid])
			if err != nil {
				return nil, err
			}
			if !shipped {
				continue
			}
		}
		objects = append(objects, LFSObject{OID: oid, Size: want.Size, Artifact: want})
	}
	return objects, nil
}

func (c *capture) putLFSObject(ctx context.Context, path string, want ArtifactRef, refs []LFSObjectRef) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		c.missing = append(c.missing, refs...)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat lfs object: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != want.Size {
		return false, fmt.Errorf("lfs object %s: %d bytes on disk, pointer says %d", path, info.Size(), want.Size)
	}
	if !c.budget.allow(want.Size) {
		for _, r := range refs {
			c.postpone(r.Path)
		}
		return false, nil
	}
	c.budget.spend(want.Size)
	//nolint:gosec // G304: path is the oid-addressed object under the worktree's own LFS store.
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open lfs object: %w", err)
	}
	defer func() { _ = f.Close() }()
	got, err := c.sink.Put(ctx, MediaLFSObject, f)
	if err != nil {
		return false, fmt.Errorf("put lfs object: %w", err)
	}
	if got != want {
		return false, fmt.Errorf("lfs object %s hashed to %s/%d", path, got.Digest, got.Size)
	}
	return true, nil
}

type pendingFile struct {
	cand   fileCandidate
	stat   fileStat
	entry  FileEntry
	target []byte
	known  bool
}

func (c *capture) captureFiles(ctx context.Context, files []fileCandidate, attrs map[string]string, fileMode bool) ([]FileEntry, error) {
	root, err := os.OpenRoot(c.wt.Root)
	if err != nil {
		return nil, fmt.Errorf("open worktree root: %w", err)
	}
	defer func() { _ = root.Close() }()
	pointers, err := c.indexPointers(ctx, files, attrs)
	if err != nil {
		return nil, err
	}
	var pending []pendingFile
	for _, f := range files {
		p, keep, err := c.prepareFile(root, f, pointers, fileMode)
		if err != nil {
			return nil, err
		}
		if keep {
			pending = append(pending, p)
		}
	}
	var known []ArtifactRef
	for _, p := range pending {
		if p.known {
			known = append(known, *p.entry.Content)
		}
	}
	has, err := c.sink.Has(ctx, known)
	if err != nil {
		return nil, fmt.Errorf("sink has: %w", err)
	}
	var entries []FileEntry
	for _, p := range pending {
		if p.known {
			held := has[0]
			has = has[1:]
			if held {
				entries = append(entries, p.entry)
				continue
			}
		}
		if !c.budget.allow(p.stat.Size) {
			c.postpone(p.cand.path)
			continue
		}
		c.budget.spend(p.stat.Size)
		ref, err := c.putFile(ctx, root, p)
		if err != nil {
			return nil, err
		}
		p.entry.Content = &ref
		entries = append(entries, p.entry)
	}
	return entries, nil
}

func (c *capture) indexPointers(ctx context.Context, files []fileCandidate, attrs map[string]string) (map[string]lfsPointer, error) {
	var oids []string
	for _, f := range files {
		if !f.untracked && attrs[f.path] == lfsFilter && blobModes[f.indexMode] {
			oids = append(oids, f.indexOID)
		}
	}
	slices.Sort(oids)
	return c.src.readPointers(ctx, slices.Compact(oids))
}

func (c *capture) prepareFile(root *os.Root, f fileCandidate, pointers map[string]lfsPointer, fileMode bool) (pendingFile, bool, error) {
	info, err := root.Lstat(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return pendingFile{}, false, fmt.Errorf("%w: %s vanished mid-capture", ErrBusy, f.path)
	}
	if err != nil {
		return pendingFile{}, false, fmt.Errorf("lstat %s: %w", f.path, err)
	}
	p := pendingFile{cand: f, stat: statOf(info), entry: FileEntry{Path: f.path, Untracked: f.untracked}}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		target, err := root.Readlink(f.path)
		if err != nil {
			return pendingFile{}, false, fmt.Errorf("readlink %s: %w", f.path, err)
		}
		p.entry.Kind, p.target, p.known = FileSymlink, []byte(target), true
		ref := contentRef([]byte(target))
		p.entry.Content = &ref
		return p, true, nil
	case mode.IsRegular():
		p.entry.Kind = FileRegular
		p.entry.Executable = mode&0o111 != 0
		if !fileMode && !f.untracked {
			p.entry.Executable = f.indexMode == "100755"
		}
	case mode.IsDir():
		return pendingFile{}, false, fmt.Errorf("%w: %s became a directory mid-capture", ErrBusy, f.path)
	default:
		c.omitted = append(c.omitted, Omission{Path: f.path, Reason: OmitSpecialFile})
		return pendingFile{}, false, nil
	}
	if cached, ok := c.prev.Files[f.path]; ok && cached.Stat == p.stat {
		ref := cached.Content
		p.entry.Content, p.known = &ref, true
	}
	ptr, isLFS := pointers[f.indexOID]
	if f.untracked || !isLFS || !ptr.Canonical {
		if p.known {
			c.next.Files[f.path] = cachedFile{Stat: p.stat, Content: *p.entry.Content}
		}
		return p, true, nil
	}
	if !p.known {
		ref, stat, err := readStable(root, f.path, p.stat, func(r io.Reader) (ArtifactRef, error) {
			h := sha256.New()
			n, err := io.Copy(h, r)
			return ArtifactRef{Digest: digestPrefix + hex.EncodeToString(h.Sum(nil)), Size: n, Media: MediaFile}, err
		})
		if err != nil {
			return pendingFile{}, false, err
		}
		p.stat, p.entry.Content, p.known = stat, &ref, true
	}
	c.next.Files[f.path] = cachedFile{Stat: p.stat, Content: *p.entry.Content}
	if ptr.OID == strings.TrimPrefix(p.entry.Content.Digest, digestPrefix) && ptr.Size == p.entry.Content.Size {
		return pendingFile{}, false, nil
	}
	return p, true, nil
}

func (c *capture) putFile(ctx context.Context, root *os.Root, p pendingFile) (ArtifactRef, error) {
	if p.entry.Kind == FileSymlink {
		ref, err := c.sink.Put(ctx, MediaFile, bytes.NewReader(p.target))
		if err != nil {
			return ArtifactRef{}, fmt.Errorf("put symlink %s: %w", p.cand.path, err)
		}
		return ref, nil
	}
	ref, stat, err := readStable(root, p.cand.path, p.stat, func(r io.Reader) (ArtifactRef, error) {
		return c.sink.Put(ctx, MediaFile, r)
	})
	if err != nil {
		return ArtifactRef{}, err
	}
	c.next.Files[p.cand.path] = cachedFile{Stat: stat, Content: ref}
	return ref, nil
}

func readStable(root *os.Root, path string, want fileStat, consume func(io.Reader) (ArtifactRef, error)) (ArtifactRef, fileStat, error) {
	for range 2 {
		ref, err := readOnce(root, path, consume)
		if err != nil {
			return ArtifactRef{}, fileStat{}, err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return ArtifactRef{}, fileStat{}, fmt.Errorf("%w: re-lstat %s: %w", ErrBusy, path, err)
		}
		now := statOf(info)
		if now == want && ref.Size == want.Size {
			return ref, now, nil
		}
		want = now
	}
	return ArtifactRef{}, fileStat{}, fmt.Errorf("%w: %s kept changing", ErrBusy, path)
}

func readOnce(root *os.Root, path string, consume func(io.Reader) (ArtifactRef, error)) (ArtifactRef, error) {
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("%w: open %s: %w", ErrBusy, path, err)
	}
	defer func() { _ = f.Close() }()
	ref, err := consume(f)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("read %s: %w", path, err)
	}
	return ref, nil
}

func contentRef(b []byte) ArtifactRef {
	sum := sha256.Sum256(b)
	return ArtifactRef{Digest: digestPrefix + hex.EncodeToString(sum[:]), Size: int64(len(b)), Media: MediaFile}
}

func statOf(info fs.FileInfo) fileStat {
	st := info.Sys().(*syscall.Stat_t)
	return fileStat{
		Ino:     st.Ino,
		Size:    st.Size,
		MtimeNS: st.Mtimespec.Nano(),
		CtimeNS: st.Ctimespec.Nano(),
		Mode:    uint32(st.Mode),
	}
}

func lstatPath(path string) (fileStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileStat{}, fmt.Errorf("lstat %s: %w", path, err)
	}
	return statOf(info), nil
}

func readSparse(gitDir string, cone bool) (*Sparse, error) {
	//nolint:gosec // G304: the sparse-checkout file of a discovered worktree's admin dir.
	b, err := os.ReadFile(filepath.Join(gitDir, "info", "sparse-checkout"))
	if err != nil {
		return nil, fmt.Errorf("read sparse-checkout: %w", err)
	}
	sparse := &Sparse{Cone: cone, Patterns: []string{}}
	for line := range strings.SplitSeq(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			sparse.Patterns = append(sparse.Patterns, line)
		}
	}
	return sparse, nil
}

func sortSnapshot(snap *Snapshot, omitted []Omission) {
	slices.SortFunc(snap.Index, func(a, b IndexEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(snap.Files, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(snap.IntentToAdd)
	slices.SortFunc(omitted, func(a, b Omission) int { return strings.Compare(a.Path, b.Path) })
	snap.Omitted = slices.CompactFunc(omitted, func(a, b Omission) bool { return a.Path == b.Path })
	snap.Complete = len(snap.Omitted) == 0
}
