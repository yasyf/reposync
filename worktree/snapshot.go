package worktree

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// SnapshotSchema identifies the snapshot manifest format.
const SnapshotSchema = "reposync-worktree-snapshot-v1"

// FileKind is the worktree state a FileEntry restores.
type FileKind string

const (
	// FileRegular is a regular file whose Content is its raw bytes.
	FileRegular FileKind = "file"
	// FileSymlink is a symlink whose Content is its target.
	FileSymlink FileKind = "symlink"
	// FileDeleted is a tracked path absent from the worktree.
	FileDeleted FileKind = "deleted"
)

// OmissionReason names WIP the capture genuinely cannot carry.
type OmissionReason string

const (
	// OmitSubmodule is a submodule with its own local changes.
	OmitSubmodule OmissionReason = "submodule"
	// OmitNestedRepo is an untracked nested git repository.
	OmitNestedRepo OmissionReason = "nested-repo"
	// OmitSpecialFile is a fifo, socket, or device.
	OmitSpecialFile OmissionReason = "special-file"
)

// Snapshot is the manifest of one worktree capture. Complete is true only when
// nothing was omitted; an incomplete snapshot must never be treated as a ready
// recovery point.
type Snapshot struct {
	Schema string `json:"schema"`
	// Digest is ContentDigest(): equal digests mean unchanged WIP.
	Digest       string    `json:"digest"`
	Source       string    `json:"source"`
	Worktree     Worktree  `json:"worktree"`
	CapturedAt   time.Time `json:"captured_at"`
	ObjectFormat string    `json:"object_format"`
	Head         Head      `json:"head"`
	// History is the bundle chain, oldest first: importing it in order yields
	// Head.Commit. It is empty exactly when Head.Ahead is zero.
	History []Bundle `json:"history,omitempty"`
	// Requires are the commits the receiver must already hold (published trunk),
	// sorted; [Head.Commit] when History is empty.
	Requires []string `json:"requires,omitempty"`
	// Index is the HEAD→index delta, sorted by path.
	Index []IndexEntry `json:"index,omitempty"`
	// Files is the index→worktree delta including untracked files, sorted by path.
	Files []FileEntry `json:"files,omitempty"`
	// IntentToAdd are the intent-to-add index entries, sorted by path.
	IntentToAdd []IntentToAdd `json:"intent_to_add,omitempty"`
	// LFSObjects are the shipped LFS objects, sorted by oid.
	LFSObjects []LFSObject `json:"lfs_objects,omitempty"`
	// LFS is set when the repository uses git-lfs.
	LFS     *LFSInfo   `json:"lfs,omitempty"`
	Sparse  *Sparse    `json:"sparse,omitempty"`
	JJ      *JJ        `json:"jj,omitempty"`
	Omitted []Omission `json:"omitted,omitempty"`
	// Complete is len(Omitted) == 0.
	Complete bool `json:"complete"`
}

// Head is the source worktree's commit position at capture.
type Head struct {
	Commit   string `json:"commit"`
	Branch   string `json:"branch,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	// TrunkTip is origin/<trunk> on the source at capture.
	TrunkTip string `json:"trunk_tip"`
	// TrunkBase is merge-base(Commit, TrunkTip).
	TrunkBase string `json:"trunk_base"`
	// Ahead is the count of commits in Commit ^TrunkTip.
	Ahead int `json:"ahead"`
}

// Bundle is one link of the history chain.
type Bundle struct {
	Artifact      ArtifactRef `json:"artifact"`
	Tip           string      `json:"tip"`
	Prerequisites []string    `json:"prerequisites,omitempty"`
}

// IndexEntry is one staged path. Mode "" means removed from the index; a
// gitlink (160000) carries its OID and no Blob.
type IndexEntry struct {
	Path string       `json:"path"`
	Mode string       `json:"mode"`
	OID  string       `json:"oid,omitempty"`
	Blob *ArtifactRef `json:"blob,omitempty"`
}

// IntentToAdd is one intent-to-add index entry. Mode is its index mode, never
// the worktree's: a deleted, hidden, or sparse intent-to-add path keeps it.
type IntentToAdd struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// FileEntry is one worktree path whose state differs from the index.
type FileEntry struct {
	Path       string       `json:"path"`
	Kind       FileKind     `json:"kind"`
	Executable bool         `json:"executable,omitempty"`
	Content    *ArtifactRef `json:"content,omitempty"`
	Untracked  bool         `json:"untracked,omitempty"`
	// AssumeUnchanged and SkipWorktree are the index flags that hid this
	// tracked path's worktree state from git status; Restore re-applies them.
	AssumeUnchanged bool `json:"assume_unchanged,omitempty"`
	SkipWorktree    bool `json:"skip_worktree,omitempty"`
}

// LFSObject is a shipped git-lfs object; Artifact.Digest is "sha256:"+OID.
type LFSObject struct {
	OID      string      `json:"oid"`
	Size     int64       `json:"size"`
	Artifact ArtifactRef `json:"artifact"`
}

// LFSObjectRef is a path referencing a git-lfs object by oid and size.
type LFSObjectRef struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// LFSInfo records a git-lfs repository's shipped object references and, as
// source metadata, the LFS remote the source's own configuration or
// .lfsconfig names. Every Objects oid is shipped in Snapshot.LFSObjects.
// Restore never reads Remote: a RestoreOptions.FetchLFS fetch selects its
// endpoint from the receiver's configuration.
type LFSInfo struct {
	Objects []LFSObjectRef `json:"objects,omitempty"`
	Remote  string         `json:"remote,omitempty"`
}

// Sparse is the source's sparse-checkout configuration.
type Sparse struct {
	Cone     bool     `json:"cone"`
	Patterns []string `json:"patterns"`
	// Exceptions are the tracked paths whose skip-worktree bit disagrees with
	// Patterns: materialized outside them, or hidden inside them.
	Exceptions []string `json:"exceptions,omitempty"`
}

func (s Sparse) equal(o Sparse) bool {
	return s.Cone == o.Cone && slices.Equal(s.Patterns, o.Patterns) && slices.Equal(s.Exceptions, o.Exceptions)
}

// JJ is informational jj state as of jj's last snapshot.
type JJ struct {
	Workspace         string   `json:"workspace"`
	ChangeID          string   `json:"change_id"`
	WorkingCopyCommit string   `json:"working_copy_commit"`
	Description       string   `json:"description,omitempty"`
	Bookmarks         []string `json:"bookmarks,omitempty"`
}

// Omission is a path the capture could not carry. Path is slash-separated with
// no trailing slash.
type Omission struct {
	Path   string         `json:"path"`
	Reason OmissionReason `json:"reason"`
}

// Summary is the catalog metadata for a snapshot.
type Summary struct {
	WorktreeID string    `json:"worktree_id"`
	Branch     string    `json:"branch,omitempty"`
	Head       string    `json:"head"`
	Digest     string    `json:"digest"`
	CapturedAt time.Time `json:"captured_at"`
	Ahead      int       `json:"ahead"`
	Staged     int       `json:"staged"`
	Unstaged   int       `json:"unstaged"`
	Untracked  int       `json:"untracked"`
	Omitted    int       `json:"omitted"`
	Bytes      int64     `json:"bytes"`
	Complete   bool      `json:"complete"`
}

// ContentDigest is the sha256 of the canonical encoding with Digest cleared
// and CapturedAt zeroed, so it changes exactly when the captured WIP does.
func (s Snapshot) ContentDigest() (string, error) {
	s.Digest = ""
	s.CapturedAt = time.Time{}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode snapshot: %w", err)
	}
	sum := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(sum[:]), nil
}

// Artifacts is the deduplicated closure of every artifact the snapshot
// references: bundles, staged blobs, file contents, and LFS objects.
func (s Snapshot) Artifacts() []ArtifactRef {
	seen := map[ArtifactRef]bool{}
	var refs []ArtifactRef
	add := func(r ArtifactRef) {
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	for _, b := range s.History {
		add(b.Artifact)
	}
	for _, e := range s.Index {
		if e.Blob != nil {
			add(*e.Blob)
		}
	}
	for _, f := range s.Files {
		if f.Content != nil {
			add(*f.Content)
		}
	}
	for _, o := range s.LFSObjects {
		add(o.Artifact)
	}
	return refs
}

// Summary condenses the snapshot for catalog metadata.
func (s Snapshot) Summary() Summary {
	sum := Summary{
		WorktreeID: s.Worktree.ID,
		Branch:     s.Head.Branch,
		Head:       s.Head.Commit,
		Digest:     s.Digest,
		CapturedAt: s.CapturedAt,
		Ahead:      s.Head.Ahead,
		Staged:     len(s.Index),
		Omitted:    len(s.Omitted),
		Complete:   s.Complete,
	}
	for _, f := range s.Files {
		if f.Untracked {
			sum.Untracked++
		} else {
			sum.Unstaged++
		}
	}
	for _, r := range s.Artifacts() {
		sum.Bytes += r.Size
	}
	return sum
}

// Encode validates s and returns its canonical JSON encoding. s.Digest must
// already equal s.ContentDigest().
func Encode(s Snapshot) ([]byte, error) {
	s.CapturedAt = s.CapturedAt.UTC()
	if err := s.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return b, nil
}

// Decode parses and validates a snapshot manifest: the schema, the worktree ID's
// derivation, object ids, artifact refs (one size per digest), sort order,
// digest, and path safety (every path clean, relative, valid UTF-8, NUL-free,
// with no ".." and no ".git" component under case folding). Failures wrap
// ErrInvalidSnapshot.
func Decode(b []byte) (Snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrInvalidSnapshot, err)
	}
	if dec.More() {
		return Snapshot{}, fmt.Errorf("%w: trailing data", ErrInvalidSnapshot)
	}
	if err := s.validate(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

func (s Snapshot) validate() error {
	if err := s.check(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSnapshot, err)
	}
	return nil
}

func (s Snapshot) check() error {
	if s.Schema != SnapshotSchema {
		return fmt.Errorf("schema %q, want %q", s.Schema, SnapshotSchema)
	}
	if s.Source == "" {
		return fmt.Errorf("empty source")
	}
	if s.CapturedAt.IsZero() {
		return fmt.Errorf("zero captured_at")
	}
	if err := s.Worktree.check(); err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	oidLen, ok := map[string]int{"sha1": 40, "sha256": 64}[s.ObjectFormat]
	if !ok {
		return fmt.Errorf("object format %q", s.ObjectFormat)
	}
	oid := func(field, v string) error {
		if !isHex(v, oidLen) {
			return fmt.Errorf("%s %q is not a %s object id", field, v, s.ObjectFormat)
		}
		return nil
	}
	checks := []error{
		oid("worktree.head", s.Worktree.Head),
		oid("head.commit", s.Head.Commit),
		oid("head.trunk_tip", s.Head.TrunkTip),
		oid("head.trunk_base", s.Head.TrunkBase),
		s.checkHistory(oid),
		s.checkIndex(oid),
		s.checkFiles(),
		s.checkLFS(),
		s.checkOmitted(),
		s.checkJJ(oid),
		s.checkArtifactSizes(),
		s.checkSparse(),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	if err := sortedUnique("intent_to_add", s.IntentToAdd, func(e IntentToAdd) string { return e.Path }); err != nil {
		return err
	}
	for i, e := range s.IntentToAdd {
		if err := checkPath(e.Path); err != nil {
			return fmt.Errorf("intent_to_add[%d]: %w", i, err)
		}
		if !blobModes[e.Mode] {
			return fmt.Errorf("intent_to_add[%d] %q: mode %q", i, e.Path, e.Mode)
		}
	}
	if s.Complete != (len(s.Omitted) == 0) {
		return fmt.Errorf("complete=%v with %d omissions", s.Complete, len(s.Omitted))
	}
	want, err := s.ContentDigest()
	if err != nil {
		return err
	}
	if s.Digest != want {
		return fmt.Errorf("digest %q, want %q", s.Digest, want)
	}
	return nil
}

func (s Snapshot) checkHistory(oid func(field, v string) error) error {
	if s.Head.Ahead < 0 {
		return fmt.Errorf("negative head.ahead %d", s.Head.Ahead)
	}
	if (s.Head.Ahead == 0) != (len(s.History) == 0) {
		return fmt.Errorf("head.ahead %d with %d history links", s.Head.Ahead, len(s.History))
	}
	for i, b := range s.History {
		if err := b.Artifact.validate(MediaBundle); err != nil {
			return fmt.Errorf("history[%d]: %w", i, err)
		}
		if err := oid(fmt.Sprintf("history[%d].tip", i), b.Tip); err != nil {
			return err
		}
		if err := sortedUnique(fmt.Sprintf("history[%d].prerequisites", i), b.Prerequisites, func(p string) string { return p }); err != nil {
			return err
		}
		for _, p := range b.Prerequisites {
			if err := oid(fmt.Sprintf("history[%d].prerequisite", i), p); err != nil {
				return err
			}
		}
	}
	if len(s.History) > 0 && s.History[len(s.History)-1].Tip != s.Head.Commit {
		return fmt.Errorf("history ends at %s, want head %s", s.History[len(s.History)-1].Tip, s.Head.Commit)
	}
	if len(s.History) == 0 && !slices.Equal(s.Requires, []string{s.Head.Commit}) {
		return fmt.Errorf("requires %v without history, want [%s]", s.Requires, s.Head.Commit)
	}
	if err := sortedUnique("requires", s.Requires, func(p string) string { return p }); err != nil {
		return err
	}
	for _, r := range s.Requires {
		if err := oid("requires", r); err != nil {
			return err
		}
	}
	return nil
}

func (s Snapshot) checkIndex(oid func(field, v string) error) error {
	if err := sortedUnique("index", s.Index, func(e IndexEntry) string { return e.Path }); err != nil {
		return err
	}
	for i, e := range s.Index {
		if err := checkPath(e.Path); err != nil {
			return fmt.Errorf("index[%d]: %w", i, err)
		}
		switch e.Mode {
		case "":
			if e.OID != "" || e.Blob != nil {
				return fmt.Errorf("index[%d] %q: removal carries content", i, e.Path)
			}
		case "160000":
			if e.Blob != nil {
				return fmt.Errorf("index[%d] %q: gitlink carries a blob", i, e.Path)
			}
			if err := oid(fmt.Sprintf("index[%d].oid", i), e.OID); err != nil {
				return err
			}
		case "100644", "100755", "120000":
			if err := oid(fmt.Sprintf("index[%d].oid", i), e.OID); err != nil {
				return err
			}
			if e.Blob == nil {
				return fmt.Errorf("index[%d] %q: missing blob", i, e.Path)
			}
			if err := e.Blob.validate(MediaBlob); err != nil {
				return fmt.Errorf("index[%d] %q: %w", i, e.Path, err)
			}
		default:
			return fmt.Errorf("index[%d] %q: mode %q", i, e.Path, e.Mode)
		}
	}
	return nil
}

func (s Snapshot) checkFiles() error {
	if err := sortedUnique("files", s.Files, func(f FileEntry) string { return f.Path }); err != nil {
		return err
	}
	for i, f := range s.Files {
		if err := checkPath(f.Path); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}
		if f.Untracked && (f.AssumeUnchanged || f.SkipWorktree) {
			return fmt.Errorf("files[%d] %q: untracked path carries index flags", i, f.Path)
		}
		switch f.Kind {
		case FileDeleted:
			if f.Content != nil || f.Executable || f.Untracked {
				return fmt.Errorf("files[%d] %q: deletion carries content, exec bit, or untracked", i, f.Path)
			}
		case FileSymlink, FileRegular:
			if f.Content == nil {
				return fmt.Errorf("files[%d] %q: missing content", i, f.Path)
			}
			if f.Kind == FileSymlink && f.Executable {
				return fmt.Errorf("files[%d] %q: executable symlink", i, f.Path)
			}
			if err := f.Content.validate(MediaFile); err != nil {
				return fmt.Errorf("files[%d] %q: %w", i, f.Path, err)
			}
		default:
			return fmt.Errorf("files[%d] %q: kind %q", i, f.Path, f.Kind)
		}
	}
	return nil
}

func (s Snapshot) checkLFS() error {
	if err := sortedUnique("lfs_objects", s.LFSObjects, func(o LFSObject) string { return o.OID }); err != nil {
		return err
	}
	shipped := map[string]int64{}
	for i, o := range s.LFSObjects {
		if !isHex(o.OID, 64) {
			return fmt.Errorf("lfs_objects[%d]: oid %q", i, o.OID)
		}
		if err := o.Artifact.validate(MediaLFSObject); err != nil {
			return fmt.Errorf("lfs_objects[%d]: %w", i, err)
		}
		if o.Artifact.Digest != digestPrefix+o.OID || o.Artifact.Size != o.Size {
			return fmt.Errorf("lfs_objects[%d]: artifact %s/%d does not match oid %s/%d", i, o.Artifact.Digest, o.Artifact.Size, o.OID, o.Size)
		}
		shipped[o.OID] = o.Size
	}
	if s.LFS == nil {
		if len(s.LFSObjects) > 0 {
			return fmt.Errorf("lfs objects without lfs info")
		}
		return nil
	}
	if err := sortedUnique("lfs.objects", s.LFS.Objects, func(r LFSObjectRef) string { return r.Path + "\x00" + r.OID }); err != nil {
		return err
	}
	referenced := map[string]bool{}
	for i, r := range s.LFS.Objects {
		if err := checkPath(r.Path); err != nil {
			return fmt.Errorf("lfs.objects[%d]: %w", i, err)
		}
		size, ok := shipped[r.OID]
		if !ok || size != r.Size {
			return fmt.Errorf("lfs.objects[%d] %q: object %s/%d is not shipped", i, r.Path, r.OID, r.Size)
		}
		referenced[r.OID] = true
	}
	if len(referenced) != len(shipped) {
		return fmt.Errorf("%d shipped lfs objects, %d referenced", len(shipped), len(referenced))
	}
	return nil
}

func (s Snapshot) checkJJ(oid func(field, v string) error) error {
	if s.JJ == nil {
		return nil
	}
	return oid("jj.working_copy_commit", s.JJ.WorkingCopyCommit)
}

func (s Snapshot) checkArtifactSizes() error {
	sizes := map[string]int64{}
	for _, r := range s.Artifacts() {
		size, seen := sizes[r.Digest]
		if seen && size != r.Size {
			return fmt.Errorf("artifact %s declared at sizes %d and %d", r.Digest, size, r.Size)
		}
		sizes[r.Digest] = r.Size
	}
	return nil
}

func (s Snapshot) checkOmitted() error {
	if err := sortedUnique("omitted", s.Omitted, func(o Omission) string { return o.Path }); err != nil {
		return err
	}
	for i, o := range s.Omitted {
		if err := checkPath(o.Path); err != nil {
			return fmt.Errorf("omitted[%d]: %w", i, err)
		}
		switch o.Reason {
		case OmitSubmodule, OmitNestedRepo, OmitSpecialFile:
		default:
			return fmt.Errorf("omitted[%d] %q: reason %q", i, o.Path, o.Reason)
		}
	}
	return nil
}

func (s Snapshot) checkSparse() error {
	if s.Sparse == nil {
		return nil
	}
	if err := sortedUnique("sparse.exceptions", s.Sparse.Exceptions, func(p string) string { return p }); err != nil {
		return err
	}
	for i, p := range s.Sparse.Exceptions {
		if err := checkPath(p); err != nil {
			return fmt.Errorf("sparse.exceptions[%d]: %w", i, err)
		}
	}
	return nil
}

func sortedUnique[T any](field string, items []T, key func(T) string) error {
	for i := 1; i < len(items); i++ {
		if key(items[i-1]) >= key(items[i]) {
			return fmt.Errorf("%s not strictly sorted at %d: %q then %q", field, i, key(items[i-1]), key(items[i]))
		}
	}
	return nil
}

func checkPath(p string) error {
	switch {
	case p == "" || p == ".":
		return fmt.Errorf("empty path")
	case !utf8.ValidString(p):
		return fmt.Errorf("path %q is not valid UTF-8", p)
	case strings.IndexByte(p, 0) >= 0:
		return fmt.Errorf("path %q contains NUL", p)
	case path.IsAbs(p) || path.Clean(p) != p:
		return fmt.Errorf("path %q is not clean and relative", p)
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == ".." || isDotGit(c) {
			return fmt.Errorf("path %q has a %q component", p, c)
		}
	}
	return nil
}

// isDotGit matches ".git" the way case-insensitive and HFS+ filesystems
// resolve it: case folded, with HFS-ignorable code points and trailing dots
// and spaces removed.
func isDotGit(component string) bool {
	stripped := strings.Map(func(r rune) rune {
		switch {
		case r == 0x200c, r == 0x200d, r == 0x200e, r == 0x200f,
			r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
			return -1
		}
		return r
	}, component)
	return strings.EqualFold(strings.TrimRight(stripped, ". "), ".git")
}
