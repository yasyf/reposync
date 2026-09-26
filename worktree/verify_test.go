package worktree_test

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
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

func TestVerifyRebuildsDamagedMirror(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "staged.txt", "only in the mirror\n")
	h.f.RunGit(h.src, "add", "staged.txt")
	snap := h.seal(h.capture())
	if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("first verify not ready: %+v", v)
	}
	oid := h.git(h.src, "rev-parse", ":staged.txt")
	if err := os.Remove(filepath.Join(h.mirrorDir(), "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("verify after damage not ready: %+v", v)
	}
	h.git(h.f.Root, "--git-dir="+h.mirrorDir(), "cat-file", "-e", oid)
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")}))
}

func TestVerifyReplacesCorruptLFSObject(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	h := newHarness(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	data := "staged lfs object\x00"
	h.f.WriteFile(h.src, "new.bin", data)
	h.f.RunGit(h.src, "add", "new.bin")
	snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
	oid := vcstest.SHA256([]byte(data))
	corrupt := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, len(data)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("verify not ready: %+v", v)
	}
	mirrored := vcstest.LFSObjectPath(h.mirrorDir(), oid)
	corrupt(mirrored)
	if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready || h.f.ReadFile(filepath.Dir(mirrored), filepath.Base(mirrored)) != data {
		t.Fatalf("verify kept a corrupt mirrored lfs object: %+v", v)
	}
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "first")}))
	corrupt(vcstest.LFSObjectPath(filepath.Join(h.recv, ".git"), oid))
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "second"), Fresh: true}))
}

func TestVerifyNeverLazyFetches(t *testing.T) {
	f := vcstest.New(t)
	f.RunGit(f.Origin, "config", "uploadpack.allowFilter", "true")
	f.RunGit(f.Origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	recv := filepath.Join(f.Root, "recv")
	f.RunGit(f.Root, "clone", "-q", "--filter=tree:0", "file://"+f.Origin, recv)
	h := newHarness(t, f, f.GitClone(filepath.Join(f.Root, "src")), recv)
	f.AdvanceOrigin("only on origin")
	h.git(h.src, "pull", "-q", "--ff-only")
	next := h.git(h.src, "rev-parse", "HEAD")
	if v := h.verify(h.seal(h.capture()), worktree.VerifyOptions{}); v.Ready || !slices.Equal(v.Missing, []string{next}) {
		t.Fatalf("verify = %+v, want not ready missing %s with no lazy fetch", v, next)
	}
}

func TestVerifyRequiresLocalBaseObjects(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *vcstest.Fixture) (recv string, lost func(src string) string, heal func(recv string))
	}{
		{"sparse blobless clone", func(t *testing.T, f *vcstest.Fixture) (string, func(string) string, func(string)) {
			if err := os.MkdirAll(filepath.Join(f.Seed, "excluded"), 0o750); err != nil {
				t.Fatal(err)
			}
			f.AdvanceOriginPath("excluded/asset.txt", "outside the sparse cone\n")
			f.RunGit(f.Origin, "config", "uploadpack.allowFilter", "true")
			f.RunGit(f.Origin, "config", "uploadpack.allowAnySHA1InWant", "true")
			recv := filepath.Join(f.Root, "recv")
			f.RunGit(f.Root, "clone", "-q", "--filter=blob:none", "--sparse", "file://"+f.Origin, recv)
			f.ConfigGit(recv)
			return recv,
				func(src string) string {
					return strings.TrimSpace(f.RunGit(src, "rev-parse", "HEAD:excluded/asset.txt"))
				},
				func(recv string) { f.RunGit(recv, "sparse-checkout", "disable") }
		}},
		{"lost base blob", func(t *testing.T, f *vcstest.Fixture) (string, func(string) string, func(string)) {
			if err := os.MkdirAll(filepath.Join(f.Seed, "lib"), 0o750); err != nil {
				t.Fatal(err)
			}
			f.AdvanceOriginPath("lib/base.txt", "untouched base file\n")
			recv := f.GitClone(filepath.Join(f.Root, "recv"))
			oid := strings.TrimSpace(f.RunGit(recv, "rev-parse", "HEAD:lib/base.txt"))
			content := f.RunGit(recv, "cat-file", "blob", oid)
			if err := os.Remove(filepath.Join(recv, ".git", "objects", oid[:2], oid[2:])); err != nil {
				t.Fatal(err)
			}
			return recv,
				func(string) string { return oid },
				func(recv string) {
					f.WriteFile(f.Root, "base.bak", content)
					f.RunGit(recv, "hash-object", "-w", filepath.Join(f.Root, "base.bak"))
				}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			recv, lost, heal := tt.setup(t, f)
			h := newHarness(t, f, f.GitClone(filepath.Join(f.Root, "src")), recv)
			h.f.WriteFile(h.src, "staged.txt", "staged\n")
			h.f.RunGit(h.src, "add", "staged.txt")
			snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
			oid := lost(h.src)

			for _, opts := range []worktree.VerifyOptions{{}, {FetchOrigin: true}} {
				if v := h.verify(snap, opts); v.Ready || !slices.Equal(v.Missing, []string{oid}) {
					t.Fatalf("verify %+v = %+v, want not ready missing [%s]", opts, v, oid)
				}
			}
			if refs := h.mirrorRefs(); len(refs) != 0 {
				t.Fatalf("mirror refs after unready verify: %v", refs)
			}
			if pins := h.pins(); len(pins) != 0 {
				t.Fatalf("pins after unready verify: %v", pins)
			}
			if got := h.git(h.recv, "rev-list", "--objects", "--missing=print", "--quiet", "HEAD^{tree}"); got != "?"+oid {
				t.Fatalf("receiver lacks %q, want only %s: verify must never fetch it", got, oid)
			}

			heal(h.recv)
			if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready || len(v.Missing) != 0 {
				t.Fatalf("verify after healing = %+v, want ready", v)
			}
			h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")}))
		})
	}
}

func TestVerifyHealsCorruptStagedBlob(t *testing.T) {
	tests := []struct {
		name    string
		between func(h *harness, snap worktree.Snapshot)
	}{
		{"cached snapshot", func(*harness, worktree.Snapshot) {}},
		{"released snapshot", func(h *harness, snap worktree.Snapshot) {
			if err := h.store.Release(h.t.Context(), h.recvReg(), snap); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.WriteFile(h.src, "staged.txt", "original staged bytes\n")
			h.f.RunGit(h.src, "add", "staged.txt")
			snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
			if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
				t.Fatalf("first verify = %+v, want ready", v)
			}
			oid := h.git(h.src, "rev-parse", ":staged.txt")
			writeLooseBlob(t, filepath.Join(h.mirrorDir(), "objects", oid[:2], oid[2:]), "CORRUPTED staged bytes\n")
			if got := h.git(h.mirrorDir(), "cat-file", "blob", oid); got != "CORRUPTED staged bytes" {
				t.Fatalf("mirror serves %q, want the corrupt bytes", got)
			}

			tt.between(h, snap)
			if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready || len(v.Missing) != 0 {
				t.Fatalf("re-verify = %+v, want ready", v)
			}
			if got := h.git(h.mirrorDir(), "cat-file", "blob", oid); got != "original staged bytes" {
				t.Fatalf("mirror serves %q after re-verify, want the staged bytes", got)
			}
			h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")}))
		})
	}
}

func TestVerifyRefusesUnhealableStagedBlob(t *testing.T) {
	tests := []struct {
		name          string
		corruptMirror bool
		release       bool
	}{
		{"mirror and receiver copies, cached snapshot", true, false},
		{"mirror and receiver copies, released snapshot", true, true},
		{"receiver copy only", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.WriteFile(h.src, "staged.txt", "original staged bytes\n")
			h.f.RunGit(h.src, "add", "staged.txt")
			snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
			if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
				t.Fatalf("first verify = %+v, want ready", v)
			}
			oid := h.git(h.src, "rev-parse", ":staged.txt")
			mirrored := filepath.Join(h.mirrorDir(), "objects", oid[:2], oid[2:])
			if tt.corruptMirror {
				writeLooseBlob(t, mirrored, "CORRUPTED mirror bytes\n")
			} else if err := os.Remove(mirrored); err != nil {
				t.Fatal(err)
			}
			writeLooseBlob(t, filepath.Join(h.recv, ".git", "objects", oid[:2], oid[2:]), "CORRUPTED receiver bytes\n")
			if tt.release {
				if err := h.store.Release(t.Context(), h.recvReg(), snap); err != nil {
					t.Fatal(err)
				}
			}

			v, err := h.store.Verify(t.Context(), h.recvReg(), snap, h.art, worktree.VerifyOptions{})
			if v.Ready || !errors.Is(err, worktree.ErrCorruptObject) {
				t.Fatalf("verify over a corrupt receiver copy of %s = %+v, %v; want not ready with ErrCorruptObject", oid, v, err)
			}
		})
	}
}

func TestVerifyAdmitsRecutBundleForMirroredTip(t *testing.T) {
	h := newGitHarness(t)
	base := h.git(h.src, "rev-parse", "origin/main")
	published := h.commit(h.src, "published.txt", "published later\n")
	tip := h.commit(h.src, "private.txt", "still private\n")
	st, wt := openStore(t), discoverAt(t, h.src, h.src)
	first := mustCapture(t, st, wt, h.art)
	if !slices.Equal(first.Requires, []string{base}) {
		t.Fatalf("first requires %v, want [%s]", first.Requires, base)
	}
	if v := h.verify(first, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("first verify = %+v, want ready", v)
	}

	h.f.RunGit(h.src, "push", "-q", "origin", published+":refs/heads/main")
	h.f.RunGit(h.recv, "pull", "-q")
	h.f.RunGit(h.src, "fetch", "-q", "origin")
	h.art.Remove(first.History[0].Artifact)
	second := mustCapture(t, st, wt, h.art)
	if len(second.History) != 1 || second.History[0].Tip != tip || second.History[0].Artifact == first.History[0].Artifact || !slices.Equal(second.Requires, []string{published}) {
		t.Fatalf("second history %+v requires %v, want tip %s recut on [%s]", second.History, second.Requires, tip, published)
	}

	if v, err := h.store.Verify(t.Context(), h.recvReg(), second, h.art, worktree.VerifyOptions{}); err != nil || !v.Ready {
		t.Fatalf("verify recut bundle for mirrored tip %s = %+v, %v; want ready", tip, v, err)
	}
	if v := h.verify(first, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("re-verify original snapshot from its cached bundle = %+v, want ready", v)
	}
	if err := h.store.Release(t.Context(), h.recvReg(), first); err != nil {
		t.Fatal(err)
	}
	if pins, want := h.pins(), slices.Sorted(slices.Values([]string{base, published})); !slices.Equal(pins, want) {
		t.Fatalf("pins = %v, want %v: the mirrored tip still builds on %s", pins, want, base)
	}
	h.assertFaithful(h.restore(second, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")}))
}

func TestVerifyRejectsUndeclaredBundlePrerequisite(t *testing.T) {
	tests := []struct {
		name  string
		prime func(h *harness, snap worktree.Snapshot) []string
	}{
		{"fresh bundle", func(*harness, worktree.Snapshot) []string { return nil }},
		{"cached tip", func(h *harness, snap worktree.Snapshot) []string {
			if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
				h.t.Fatalf("genuine verify = %+v, want ready", v)
			}
			h.art.Remove(snap.History[0].Artifact)
			if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
				h.t.Fatalf("re-verify without the cached bundle = %+v, want ready", v)
			}
			return snap.Requires
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.commit(h.src, "feature.txt", "private history\n")
			snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
			prereq := h.git(h.src, "rev-parse", "origin/main")
			if !slices.Equal(snap.History[0].Prerequisites, []string{prereq}) || !slices.Equal(snap.Requires, []string{prereq}) {
				t.Fatalf("captured prerequisites %v requires %v, want [%s]", snap.History[0].Prerequisites, snap.Requires, prereq)
			}
			wantPins := tt.prime(h, snap)

			tampered := snap
			tampered.Requires = nil
			tampered = h.seal(tampered)
			v, err := h.store.Verify(t.Context(), h.recvReg(), tampered, h.art, worktree.VerifyOptions{})
			if !errors.Is(err, worktree.ErrUndeclaredPrerequisite) || v.Ready {
				t.Fatalf("verify with undeclared prerequisite %s = %+v, %v; want not ready with ErrUndeclaredPrerequisite", prereq, v, err)
			}
			if pins := h.pins(); !slices.Equal(pins, wantPins) {
				t.Fatalf("pins = %v, want %v", pins, wantPins)
			}
			if ref, ok := h.mirrorRefs()[worktree.SnapshotPrefix+worktree.SnapshotKey(tampered)+"/head"]; ok {
				t.Fatalf("rejected snapshot recorded in the mirror at %s", ref)
			}
		})
	}
}

func TestVerifyPinsSurviveReceiverGC(t *testing.T) {
	h := newGitHarness(t)
	h.commit(h.src, "feature.txt", "private history\n")
	st, wt := openStore(t), discoverAt(t, h.src, h.src)
	first := mustCapture(t, st, wt, h.art)
	h.f.WriteFile(h.src, "wip.txt", "reuses the cached tip\n")
	second := mustCapture(t, st, wt, h.art)
	if len(second.History) != 1 || second.History[0].Tip != first.History[0].Tip || second.History[0].Artifact != first.History[0].Artifact {
		t.Fatalf("second history %+v does not reuse %+v", second.History, first.History)
	}
	prereq := h.git(h.src, "rev-parse", "origin/main")

	if v := h.verify(first, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("first verify = %+v, want ready", v)
	}
	h.art.Remove(first.History[0].Artifact)
	if v := h.verify(second, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("verify reusing the cached tip without its bundle = %+v, want ready", v)
	}
	if err := h.store.Release(t.Context(), h.recvReg(), first); err != nil {
		t.Fatal(err)
	}
	if pins := h.pins(); !slices.Equal(pins, []string{prereq}) {
		t.Fatalf("pins = %v, want [%s]", pins, prereq)
	}

	root := h.git(h.recv, "commit-tree", "HEAD^{tree}", "-m", "new root")
	h.git(h.recv, "update-ref", "refs/heads/main", root)
	h.git(h.recv, "update-ref", "refs/remotes/origin/main", root)
	h.git(h.recv, "reflog", "expire", "--expire=now", "--all")
	h.git(h.recv, "gc", "-q", "--prune=now")
	h.git(h.recv, "cat-file", "-e", prereq+"^{commit}")
	h.assertFaithful(h.restore(second, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")}))
}

func writeLooseBlob(t *testing.T, path, content string) {
	t.Helper()
	var encoded bytes.Buffer
	zw := zlib.NewWriter(&encoded)
	if _, err := fmt.Fprintf(zw, "blob %d\x00%s", len(content), content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
