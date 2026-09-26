package worktree_test

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/registry"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

type harness struct {
	t     *testing.T
	f     *vcstest.Fixture
	src   string
	recv  string
	store *worktree.Store
	art   *worktreetest.Store
}

func newHarness(t *testing.T, f *vcstest.Fixture, src, recv string) *harness {
	t.Helper()
	store, err := worktree.OpenStore(filepath.Join(f.Root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, f: f, src: src, recv: recv, store: store, art: worktreetest.New()}
}

func newGitHarness(t *testing.T) *harness {
	t.Helper()
	f := vcstest.New(t)
	return newHarness(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
}

func (h *harness) reg(path string) registry.Registry {
	return registry.Registry{Repos: []registry.Repo{{Relpath: "wt", Path: path, Origin: testOrigin, Trunk: "main"}}}
}

func (h *harness) recvReg() registry.Registry { return h.reg(h.recv) }

func (h *harness) git(dir string, args ...string) string {
	h.t.Helper()
	return strings.TrimSpace(h.f.RunGit(dir, args...))
}

func (h *harness) source() worktree.TestSource {
	return worktree.TestSource{T: h.t, F: h.f, Root: h.src, Reg: h.reg(h.src), Sink: h.art}
}

func (h *harness) put(media worktree.Media, data []byte) *worktree.ArtifactRef {
	h.t.Helper()
	return h.source().Put(media, data)
}

func (h *harness) capture() worktree.Snapshot {
	h.t.Helper()
	return h.source().Capture()
}

func (h *harness) seal(s worktree.Snapshot) worktree.Snapshot {
	h.t.Helper()
	return worktree.Seal(h.t, s)
}

func (h *harness) commit(dir, path, content string) string {
	h.t.Helper()
	h.f.WriteFile(dir, path, content)
	h.f.RunGit(dir, "add", path)
	h.f.RunGit(dir, "commit", "-qm", "edit "+path)
	return h.git(dir, "rev-parse", "HEAD")
}

func (h *harness) mirrorDir() string {
	return h.store.MirrorDir(testOrigin)
}

func (h *harness) refs(gitArgs []string, prefix string) map[string]string {
	h.t.Helper()
	refs, err := worktree.ListRefs(h.t.Context(), gitArgs, prefix)
	if err != nil {
		h.t.Fatal(err)
	}
	return refs
}

func (h *harness) mirrorRefs() map[string]string {
	h.t.Helper()
	if _, err := os.Stat(h.mirrorDir()); errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}
	}
	return h.refs([]string{"--git-dir=" + h.mirrorDir()}, "refs/")
}

func (h *harness) pins() []string {
	h.t.Helper()
	refs := h.refs([]string{"-C", h.recv}, worktree.PinPrefix)
	pins := make([]string, 0, len(refs))
	for ref := range refs {
		pins = append(pins, strings.TrimPrefix(ref, worktree.PinPrefix))
	}
	slices.Sort(pins)
	return pins
}

func (h *harness) verify(snap worktree.Snapshot, opts worktree.VerifyOptions) worktree.Verification {
	h.t.Helper()
	v, err := h.store.Verify(h.t.Context(), h.recvReg(), snap, h.art, opts)
	if err != nil {
		h.t.Fatalf("verify: %v", err)
	}
	return v
}

func TestVerifyRequiresTrunk(t *testing.T) {
	h := newGitHarness(t)
	trunk := h.f.AdvanceOrigin("trunk moves")
	h.f.RunGit(h.src, "pull", "-q")
	h.commit(h.src, "feature.txt", "feature\n")
	snap := h.seal(h.capture())
	if !slices.Equal(snap.Requires, []string{trunk}) {
		t.Fatalf("requires = %v, want [%s]", snap.Requires, trunk)
	}

	v := h.verify(snap, worktree.VerifyOptions{})
	if v.Ready || !slices.Equal(v.Missing, []string{trunk}) || v.Checkout != h.recv {
		t.Fatalf("behind trunk: %+v, want not ready missing [%s] in %s", v, trunk, h.recv)
	}
	if refs := h.mirrorRefs(); len(refs) != 0 {
		t.Fatalf("mirror refs after unready verify: %v", refs)
	}
	if pins := h.pins(); len(pins) != 0 {
		t.Fatalf("pins after unready verify: %v", pins)
	}

	v = h.verify(snap, worktree.VerifyOptions{FetchOrigin: true})
	if !v.Ready || len(v.Missing) != 0 {
		t.Fatalf("after origin fetch: %+v, want ready", v)
	}
	if pins := h.pins(); !slices.Equal(pins, []string{trunk}) {
		t.Fatalf("pins = %v, want [%s]", pins, trunk)
	}
	refs := h.mirrorRefs()
	key := worktree.SnapshotKey(snap)
	if refs[worktree.TipPrefix+snap.Head.Commit] != snap.Head.Commit || refs[worktree.SnapshotPrefix+key+"/head"] != snap.Head.Commit || refs[worktree.SnapshotPrefix+key+"/index"] == "" {
		t.Fatalf("mirror refs = %v", refs)
	}
}

func TestVerifyRejectsBadArtifacts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(h *harness, s worktree.Snapshot) worktree.Snapshot
	}{
		{"corrupt bundle", func(h *harness, s worktree.Snapshot) worktree.Snapshot {
			h.art.Corrupt(s.History[0].Artifact)
			return s
		}},
		{"truncated bundle", func(h *harness, s worktree.Snapshot) worktree.Snapshot {
			h.art.Truncate(s.History[0].Artifact, int(s.History[0].Artifact.Size/2))
			return s
		}},
		{"corrupt staged blob", func(h *harness, s worktree.Snapshot) worktree.Snapshot {
			h.art.Corrupt(*s.Index[0].Blob)
			return s
		}},
		{"staged blob oid mismatch", func(h *harness, s worktree.Snapshot) worktree.Snapshot {
			s.Index[0].OID = h.git(h.src, "rev-parse", "HEAD:README.md")
			return h.seal(s)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.commit(h.src, "feature.txt", "feature\n")
			h.f.WriteFile(h.src, "staged.txt", "staged\n")
			h.f.RunGit(h.src, "add", "staged.txt")
			snap := tt.mutate(h, h.seal(h.capture()))
			_, err := h.store.Verify(t.Context(), h.recvReg(), snap, h.art, worktree.VerifyOptions{})
			if !errors.Is(err, worktree.ErrArtifactMismatch) {
				t.Fatalf("verify error = %v, want ErrArtifactMismatch", err)
			}
			if refs := h.mirrorRefs(); len(refs) != 0 {
				t.Fatalf("mirror refs after failed verify: %v", refs)
			}
			if pins := h.pins(); len(pins) != 0 {
				t.Fatalf("pins after failed verify: %v", pins)
			}
		})
	}
}

func TestVerifyNotReady(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())
	h.art.Remove(*snap.Files[0].Content)
	if v := h.verify(snap, worktree.VerifyOptions{}); v.Ready || !slices.Equal(v.Missing, []string{snap.Files[0].Content.Digest}) {
		t.Fatalf("missing file artifact: %+v", v)
	}

	h.f.WriteFile(h.src, "wip.txt", "wip again\n")
	incomplete := h.capture()
	incomplete.Omitted = []worktree.Omission{{Path: "pipe", Reason: worktree.OmitSpecialFile}}
	incomplete = h.seal(incomplete)
	if v := h.verify(incomplete, worktree.VerifyOptions{}); v.Ready || !slices.Equal(v.Missing, []string{"omitted:special-file:pipe"}) {
		t.Fatalf("incomplete snapshot: %+v, want never ready", v)
	}
	if refs := h.mirrorRefs(); len(refs) != 0 {
		t.Fatalf("mirror refs after unready verify: %v", refs)
	}
}

func TestVerifyPinsIdempotenceAndRelease(t *testing.T) {
	h := newGitHarness(t)
	ctx := t.Context()
	h.commit(h.src, "feature.txt", "feature\n")
	h.f.WriteFile(h.src, "wip.txt", "one\n")
	first := h.seal(h.capture())
	if v := h.verify(first, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("first: %+v", v)
	}
	refs := h.mirrorRefs()
	if v := h.verify(first, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("re-verify: %+v", v)
	}
	if again := h.mirrorRefs(); !maps.Equal(refs, again) {
		t.Fatalf("re-verify changed refs: %v -> %v", refs, again)
	}

	h.f.WriteFile(h.src, "wip.txt", "two\n")
	second := h.seal(h.capture())
	if v := h.verify(second, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("second: %+v", v)
	}
	if err := h.store.Release(ctx, h.recvReg(), first); err != nil {
		t.Fatal(err)
	}
	if pins := h.pins(); !slices.Equal(pins, first.Requires) {
		t.Fatalf("pins after releasing one of two = %v, want %v", pins, first.Requires)
	}
	refs = h.mirrorRefs()
	if _, ok := refs[worktree.SnapshotPrefix+worktree.SnapshotKey(first)+"/head"]; ok {
		t.Fatalf("released snapshot refs remain: %v", refs)
	}
	if refs[worktree.TipPrefix+second.Head.Commit] == "" || refs[worktree.SnapshotPrefix+worktree.SnapshotKey(second)+"/index"] == "" {
		t.Fatalf("retained snapshot refs missing: %v", refs)
	}

	objects := filepath.Join(h.mirrorDir(), "objects")
	entries, err := os.ReadDir(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "info" && e.Name() != "pack" {
			if err := os.RemoveAll(filepath.Join(objects, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(objects, "pack")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(objects, "pack"), 0o750); err != nil {
		t.Fatal(err)
	}
	if v := h.verify(second, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("verify after mirror object loss: %+v", v)
	}
	h.f.RunGit(h.f.Root, "--git-dir="+h.mirrorDir(), "cat-file", "-e", worktree.SnapshotPrefix+worktree.SnapshotKey(second)+"/index^{tree}")
	h.f.RunGit(h.f.Root, "--git-dir="+h.mirrorDir(), "cat-file", "-e", second.Head.Commit)

	if err := h.store.Release(ctx, h.recvReg(), second); err != nil {
		t.Fatal(err)
	}
	if pins := h.pins(); len(pins) != 0 {
		t.Fatalf("pins after releasing all = %v", pins)
	}
	if refs := h.mirrorRefs(); len(refs) != 0 {
		t.Fatalf("mirror refs after releasing all = %v", refs)
	}
}
