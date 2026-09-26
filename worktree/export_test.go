package worktree

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/registry"
)

const (
	PinPrefix      = pinPrefix
	TipPrefix      = tipPrefix
	SnapshotPrefix = snapshotPrefix
	RecoveryPrefix = recoveryPrefix
)

var (
	CheckGitVersion = checkGitVersion
	ListRefs        = listRefs
	Folding         = folding
	SnapshotKey     = snapshotKey
)

func (s *Store) MirrorDir(origin string) string {
	return s.mirrorDir(origin)
}

// TestSource hand-builds snapshots of a source checkout's main worktree with
// plain git reads, standing in for the capture lane.
type TestSource struct {
	T    *testing.T
	F    *vcstest.Fixture
	Root string
	Reg  registry.Registry
	Sink ArtifactSink
}

func (c TestSource) git(args ...string) string {
	c.T.Helper()
	return strings.TrimSpace(c.F.RunGit(c.Root, args...))
}

func (c TestSource) Put(media Media, data []byte) *ArtifactRef {
	c.T.Helper()
	ref, err := c.Sink.Put(c.T.Context(), media, bytes.NewReader(data))
	if err != nil {
		c.T.Fatal(err)
	}
	return &ref
}

func (c TestSource) Capture() Snapshot {
	c.T.Helper()
	ctx := c.T.Context()
	wts, _, err := Discover(ctx, c.Reg)
	if err != nil || len(wts) == 0 {
		c.T.Fatalf("discover source: %v %v", wts, err)
	}
	st, err := readStatus(ctx, c.Root, nil)
	if err != nil {
		c.T.Fatal(err)
	}
	head, trunk := c.git("rev-parse", "HEAD"), c.git("rev-parse", "origin/main")
	ahead, err := strconv.Atoi(c.git("rev-list", "--count", head, "^"+trunk))
	if err != nil {
		c.T.Fatal(err)
	}
	snap := Snapshot{
		Schema:       SnapshotSchema,
		Source:       "hostA",
		Worktree:     wts[0],
		CapturedAt:   time.Now().UTC(),
		ObjectFormat: c.git("rev-parse", "--show-object-format"),
		Head: Head{
			Commit: head, Branch: st.branch, Upstream: st.upstream,
			TrunkTip: trunk, TrunkBase: c.git("merge-base", head, trunk), Ahead: ahead,
		},
		Requires: []string{head},
	}
	if ahead > 0 {
		snap.History, snap.Requires = c.bundle(head, trunk)
	}
	for _, e := range st.changed {
		if e.x != '.' {
			ie := IndexEntry{Path: e.path}
			if e.modeIndex != "000000" {
				ie.Mode, ie.OID = e.modeIndex, e.oidIndex
				ie.Blob = c.Put(MediaBlob, []byte(c.F.RunGit(c.Root, "cat-file", "blob", e.oidIndex)))
			}
			snap.Index = append(snap.Index, ie)
		}
		if e.y != '.' {
			snap.Files = append(snap.Files, c.fileEntry(e.path, false))
		}
		if e.y == 'A' {
			snap.IntentToAdd = append(snap.IntentToAdd, e.path)
		}
	}
	for _, p := range st.untracked {
		snap.Files = append(snap.Files, c.fileEntry(p, true))
	}
	slices.SortFunc(snap.Files, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	return snap
}

func (c TestSource) bundle(head, trunk string) ([]Bundle, []string) {
	c.T.Helper()
	path := filepath.Join(c.T.TempDir(), "history.bundle")
	c.F.RunGit(c.Root, "bundle", "create", "-q", path, "HEAD", "^"+trunk)
	//nolint:gosec // G304: test helper reading a bundle it just wrote under a test temp dir.
	data, err := os.ReadFile(path)
	if err != nil {
		c.T.Fatal(err)
	}
	header, _, _ := bytes.Cut(data, []byte("\n\n"))
	var prereqs []string
	for l := range strings.SplitSeq(string(header), "\n") {
		if oid, ok := strings.CutPrefix(l, "-"); ok {
			prereqs = append(prereqs, strings.Fields(oid)[0])
		}
	}
	slices.Sort(prereqs)
	return []Bundle{{Artifact: *c.Put(MediaBundle, data), Tip: head, Prerequisites: prereqs}}, prereqs
}

func (c TestSource) fileEntry(p string, untracked bool) FileEntry {
	c.T.Helper()
	full := filepath.Join(c.Root, filepath.FromSlash(p))
	info, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return FileEntry{Path: p, Kind: FileDeleted}
	}
	if err != nil {
		c.T.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			c.T.Fatal(err)
		}
		return FileEntry{Path: p, Kind: FileSymlink, Content: c.Put(MediaFile, []byte(target)), Untracked: untracked}
	}
	//nolint:gosec // G304: test helper reading a file under a test-controlled source checkout.
	data, err := os.ReadFile(full)
	if err != nil {
		c.T.Fatal(err)
	}
	return FileEntry{Path: p, Kind: FileRegular, Executable: info.Mode()&0o111 != 0, Content: c.Put(MediaFile, data), Untracked: untracked}
}

// Seal sets Complete and Digest and proves the hand-built snapshot encodes.
func Seal(t *testing.T, s Snapshot) Snapshot {
	t.Helper()
	s.Complete = len(s.Omitted) == 0
	digest, err := s.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	s.Digest = digest
	if _, err := Encode(s); err != nil {
		t.Fatalf("hand-built snapshot is invalid: %v", err)
	}
	return s
}
