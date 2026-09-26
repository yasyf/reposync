package worktree_test

import (
	"bytes"
	"cmp"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

const lfsBase = "base asset\x00\x01"

type expectation struct {
	files  map[string]vcstest.TreeEntry
	status []string
	log    string
}

type roundTrip struct {
	*harness
	srcStore *worktree.Store
}

func newRoundTrip(t *testing.T, f *vcstest.Fixture, src, recv string) *roundTrip {
	t.Helper()
	srcStore, err := worktree.OpenStore(filepath.Join(f.Root, "source-store"))
	if err != nil {
		t.Fatal(err)
	}
	return &roundTrip{harness: newHarness(t, f, src, recv), srcStore: srcStore}
}

func (rt *roundTrip) tick() (worktree.Snapshot, expectation) {
	rt.t.Helper()
	wt := discoverAt(rt.t, rt.src, rt.src)
	guarded := []string{wt.CommonDir}
	if wt.GitDir != wt.CommonDir {
		guarded = append(guarded, wt.GitDir)
	}
	if _, err := os.Stat(filepath.Join(wt.Root, ".jj")); err == nil {
		guarded = append(guarded, filepath.Join(wt.Root, ".jj"))
	}
	before := trees(rt.f, guarded...)
	snap := mustCapture(rt.t, rt.srcStore, wt, rt.art)
	if after := trees(rt.f, guarded...); !maps.Equal(before, after) {
		rt.t.Fatalf("capture wrote the source repository: %q", changed(before, after))
	}
	enc, err := worktree.Encode(snap)
	if err != nil {
		rt.t.Fatal(err)
	}
	decoded, err := worktree.Decode(enc)
	if err != nil {
		rt.t.Fatalf("Decode(captured snapshot): %v", err)
	}
	return decoded, expectation{files: rt.worktreeFiles(rt.src), status: rt.status(rt.src), log: rt.git(rt.src, "log", "--format=%H")}
}

func (rt *roundTrip) worktreeFiles(dir string) map[string]vcstest.TreeEntry {
	tree := rt.files(dir)
	maps.DeleteFunc(tree, func(p string, _ vcstest.TreeEntry) bool {
		return strings.HasPrefix(p, ".jj/") || strings.HasPrefix(p, "node_modules/") || strings.HasSuffix(p, ".log")
	})
	return tree
}

func (rt *roundTrip) pickup(store *worktree.Store, recv string, src worktree.ArtifactSource, snap worktree.Snapshot, opts worktree.RestoreOptions) worktree.Restored {
	rt.t.Helper()
	reg := rt.reg(recv)
	v, err := store.Verify(rt.t.Context(), reg, snap, src, worktree.VerifyOptions{})
	if err != nil || !v.Ready || len(v.Missing) != 0 {
		rt.t.Fatalf("verify: %+v, %v", v, err)
	}
	r, err := store.Restore(rt.t.Context(), reg, snap, src, opts)
	if err != nil {
		rt.t.Fatalf("restore: %v", err)
	}
	return r
}

func (rt *roundTrip) assertRestored(snap worktree.Snapshot, r worktree.Restored, want expectation) {
	rt.t.Helper()
	branch := "recovery/" + cmp.Or(snap.Head.Branch, "detached") + "-host-a-"
	if !r.Exact || len(r.Differences) != 0 || len(r.LFSPending) != 0 || r.Reused || r.Head != snap.Head.Commit || r.Applied != snap.Digest || !strings.HasPrefix(r.Branch, branch) {
		rt.t.Fatalf("restored = %+v, want an exact %s* checkout of %s", r, branch, snap.Head.Commit)
	}
	if got := rt.worktreeFiles(r.Path); !maps.Equal(want.files, got) {
		rt.t.Fatalf("worktree bytes or modes differ at %q", changed(want.files, got))
	}
	if got := rt.status(r.Path); !slices.Equal(want.status, got) {
		rt.t.Fatalf("status differs:\nsource   %q\nrestored %q", want.status, got)
	}
	if got := rt.git(r.Path, "log", "--format=%H"); got != want.log {
		rt.t.Fatalf("history differs:\nsource   %s\nrestored %s", want.log, got)
	}
	for _, ignored := range []string{"node_modules", "debug.log", "dir/trace.log"} {
		if _, err := os.Lstat(filepath.Join(r.Path, ignored)); !errors.Is(err, fs.ErrNotExist) {
			rt.t.Fatalf("ignored %s restored: %v", ignored, err)
		}
	}
}

func changed(want, got map[string]vcstest.TreeEntry) []string {
	var diff []string
	for p, e := range want {
		if g, ok := got[p]; !ok || g != e {
			diff = append(diff, p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			diff = append(diff, p)
		}
	}
	slices.Sort(diff)
	return diff
}

func writeBytes(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func lfsOIDs(snap worktree.Snapshot) []string {
	oids := make([]string, 0, len(snap.LFSObjects))
	for _, o := range snap.LFSObjects {
		oids = append(oids, o.OID)
	}
	return oids
}

func fileEntry(snap worktree.Snapshot, path string) (worktree.FileEntry, bool) {
	i := slices.IndexFunc(snap.Files, func(e worktree.FileEntry) bool { return e.Path == path })
	if i < 0 {
		return worktree.FileEntry{}, false
	}
	return snap.Files[i], true
}

func TestRoundTrip(t *testing.T) {
	gitRepos := func(f *vcstest.Fixture) (string, string) {
		return f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv"))
	}
	lfsRepos := func(f *vcstest.Fixture) (string, string) {
		f.EnableLFS("*.bin")
		f.AdvanceOriginPath("base.bin", lfsBase)
		return f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv"))
	}
	tests := []struct {
		name  string
		repos func(f *vcstest.Fixture) (src, recv string)
		wip   func(rt *roundTrip)
		check func(t *testing.T, snap worktree.Snapshot)
	}{
		{"detached-head", gitRepos, func(rt *roundTrip) {
			rt.commit(rt.src, "one.txt", "one\n")
			rt.f.RunGit(rt.src, "checkout", "-q", "--detach")
			rt.commit(rt.src, "two.txt", "two\n")
			rt.f.WriteFile(rt.src, "README.md", "detached edit\n")
			rt.f.WriteFile(rt.src, "loose.txt", "untracked\n")
		}, func(t *testing.T, snap worktree.Snapshot) {
			if snap.Worktree.Kind != worktree.KindGit || snap.Head.Branch != "" || snap.Head.Ahead != 2 || len(snap.History) != 1 {
				t.Fatalf("head %+v kind %s history %d", snap.Head, snap.Worktree.Kind, len(snap.History))
			}
		}},
		{"jj-colocated-into-plain-git", func(f *vcstest.Fixture) (string, string) {
			return f.JJClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv"))
		}, func(rt *roundTrip) {
			rt.f.WriteFile(rt.src, "one.txt", "one\n")
			rt.f.RunJJ(rt.src, "commit", "-m", "one")
			rt.f.WriteFile(rt.src, "two.txt", "two\n")
			rt.f.RunJJ(rt.src, "commit", "-m", "two")
			rt.f.RunJJ(rt.src, "describe", "-m", "wip description")
			rt.f.WriteFile(rt.src, "README.md", "unsnapshotted edit\n")
			rt.f.WriteFile(rt.src, "fresh.txt", "unsnapshotted new file\n")
		}, func(t *testing.T, snap worktree.Snapshot) {
			if snap.Worktree.Kind != worktree.KindJJColocated || snap.JJ == nil || !strings.Contains(snap.JJ.Description, "wip description") || snap.Head.Ahead != 2 {
				t.Fatalf("kind %s jj %+v head %+v", snap.Worktree.Kind, snap.JJ, snap.Head)
			}
			if got := filePaths(snap); !slices.Equal(got, []string{"file:README.md", "file:fresh.txt"}) {
				t.Fatalf("files %v, want the unsnapshotted edits", got)
			}
		}},
		{"lfs-unchanged-is-complete", lfsRepos, func(rt *roundTrip) {
			rt.f.WriteFile(rt.src, "README.md", "plain edit in an lfs repo\n")
		}, func(t *testing.T, snap worktree.Snapshot) {
			if snap.LFS == nil || len(snap.LFSObjects) != 0 || !slices.Equal(filePaths(snap), []string{"file:README.md"}) {
				t.Fatalf("lfs %+v objects %v files %v", snap.LFS, snap.LFSObjects, filePaths(snap))
			}
		}},
		{"lfs-modified-binary", lfsRepos, func(rt *roundTrip) {
			rt.f.WriteFile(rt.src, "base.bin", "edited asset\x00\x02")
		}, func(t *testing.T, snap worktree.Snapshot) {
			e, ok := fileEntry(snap, "base.bin")
			if !ok || e.Content.Media != worktree.MediaFile || e.Content.Size != int64(len("edited asset\x00\x02")) || len(snap.LFSObjects) != 0 {
				t.Fatalf("base.bin %+v objects %v", e, snap.LFSObjects)
			}
		}},
		{"lfs-staged-new-file", lfsRepos, func(rt *roundTrip) {
			rt.f.WriteFile(rt.src, "new.bin", "staged asset\x00\x03")
			rt.f.WriteFile(rt.src, "glob [1]*?.bin", "glob-named asset\x00\x05")
			rt.f.RunGit(rt.src, "add", "new.bin", ":(literal)glob [1]*?.bin")
		}, func(t *testing.T, snap worktree.Snapshot) {
			want := []string{vcstest.SHA256([]byte("staged asset\x00\x03")), vcstest.SHA256([]byte("glob-named asset\x00\x05"))}
			slices.Sort(want)
			if got := lfsOIDs(snap); len(snap.Index) != 2 || !slices.Equal(got, want) {
				t.Fatalf("index %+v lfs objects %v", snap.Index, got)
			}
		}},
		{"lfs-unpushed-commit-hydrates-offline", lfsRepos, func(rt *roundTrip) {
			rt.commit(rt.src, "feature.bin", "unpushed asset\x00\x04")
		}, func(t *testing.T, snap worktree.Snapshot) {
			if got := lfsOIDs(snap); snap.Head.Ahead != 1 || !slices.Equal(got, []string{vcstest.SHA256([]byte("unpushed asset\x00\x04"))}) {
				t.Fatalf("ahead %d lfs objects %v", snap.Head.Ahead, got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			src, recv := tt.repos(f)
			rt := newRoundTrip(t, f, src, recv)
			tt.wip(rt)
			snap, want := rt.tick()
			if !snap.Complete || len(snap.Omitted) != 0 {
				t.Fatalf("complete=%v omitted=%v", snap.Complete, snap.Omitted)
			}
			tt.check(t, snap)
			if err := os.RemoveAll(filepath.Join(f.Origin, "lfs")); err != nil {
				t.Fatal(err)
			}
			rt.assertRestored(snap, rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")}), want)
		})
	}
}

func TestRoundTripWIPMatrix(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	for p, c := range map[string]string{
		"a.txt": "a1\n", "bin.dat": "\x00\x01bin1", "del-staged.txt": "x\n", "del-unstaged.txt": "y\n",
		"run.sh": "#!/bin/sh\n", ".gitignore": "node_modules/\n*.log\n",
	} {
		f.WriteFile(f.Seed, p, c)
	}
	if err := os.Symlink("a.txt", filepath.Join(f.Seed, "link")); err != nil {
		t.Fatal(err)
	}
	f.RunGit(f.Seed, "add", "-A")
	f.RunGit(f.Seed, "commit", "-qm", "published base")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))

	rt.commit(rt.src, "one.txt", "first unpushed commit\n")
	f.WriteFile(rt.src, "a.txt", "tick one\n")
	first, want := rt.tick()
	if !first.Complete || first.Head.Ahead != 1 || len(first.History) != 1 {
		t.Fatalf("first tick: complete=%v head %+v history %d", first.Complete, first.Head, len(first.History))
	}
	one := filepath.Join(f.Root, "one")
	rt.assertRestored(first, rt.pickup(rt.store, rt.recv, rt.art, first, worktree.RestoreOptions{Dest: one}), want)
	f.WriteFile(one, "a.txt", "edited on the receiver\n")

	rt.commit(rt.src, "two.txt", "second unpushed commit\n")
	f.WriteFile(rt.src, "a.txt", "a2 staged\n")
	f.WriteFile(rt.src, "bin.dat", "\x00\x02bin2 staged")
	f.RunGit(rt.src, "add", "a.txt", "bin.dat")
	f.WriteFile(rt.src, "a.txt", "a3 worktree\n")
	f.WriteFile(rt.src, "bin.dat", "\x00\x03bin3 worktree")
	f.RunGit(rt.src, "rm", "-q", "del-staged.txt")
	f.WriteFile(rt.src, "build.gen", "generated v2\n")
	for _, step := range []func() error{
		func() error { return os.Remove(filepath.Join(rt.src, "del-unstaged.txt")) },
		//nolint:gosec // G302: the fixture needs an executable file to prove the exec bit round-trips.
		func() error { return os.Chmod(filepath.Join(rt.src, "run.sh"), 0o755) },
		func() error { return os.Remove(filepath.Join(rt.src, "link")) },
		func() error { return os.Symlink("bin.dat", filepath.Join(rt.src, "link")) },
		func() error { return os.Symlink("a.txt", filepath.Join(rt.src, "untracked-link")) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	f.WriteFile(rt.src, "ita.txt", "intent\n")
	f.RunGit(rt.src, "add", "-N", "ita.txt")
	big := make([]byte, 9<<20)
	for i := range big {
		big[i] = byte(i % 251)
	}
	for name, data := range map[string][]byte{
		"dir/sub/new.txt":                      []byte("nested\n"),
		"dir/ünï cöde\ttab.txt":                []byte("unicode\n"),
		"dir/new\nline.txt":                    []byte("newline\n"),
		"dir/punct !#$%&'()+,;=@[]^`{}~*?.txt": []byte("punctuation\n"),
		"big.dat":                              big,
		"node_modules/pkg/index.js":            []byte("ignored\n"),
		"debug.log":                            []byte("ignored\n"),
		"dir/trace.log":                        []byte("ignored\n"),
	} {
		writeBytes(t, rt.src, name, data)
	}

	second, want := rt.tick()
	if !second.Complete || second.Head.Ahead != 2 || len(second.History) != 2 || second.History[0].Tip != first.History[0].Tip || second.History[0].Artifact != first.History[0].Artifact {
		t.Fatalf("second tick: complete=%v head %+v history %+v, want the first link reused", second.Complete, second.Head, second.History)
	}
	if !slices.Equal(second.IntentToAdd, []string{"ita.txt"}) {
		t.Fatalf("intent to add %v", second.IntentToAdd)
	}
	if e, ok := fileEntry(second, "big.dat"); !ok || !e.Untracked || e.Content.Size != int64(len(big)) {
		t.Fatalf("big.dat %+v", e)
	}
	if e, ok := fileEntry(second, "build.gen"); !ok || e.Untracked {
		t.Fatalf("tracked generated edit %+v not captured", e)
	}
	for _, p := range filePaths(second) {
		if strings.Contains(p, "node_modules/") || strings.HasSuffix(p, ".log") {
			t.Fatalf("ignored %s captured", p)
		}
	}

	r := rt.pickup(rt.store, rt.recv, rt.art, second, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "two")})
	if !r.Reused || !r.Newer || r.Path != one || r.Applied != first.Digest {
		t.Fatalf("repeated pickup = %+v, want reuse of %s", r, one)
	}
	if got := f.ReadFile(one, "a.txt"); got != "edited on the receiver\n" {
		t.Fatalf("repeated pickup rolled back the sibling: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(f.Root, "two")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("repeated pickup created a checkout: %v", err)
	}
	rt.assertRestored(second, rt.pickup(rt.store, rt.recv, rt.art, second, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "three"), Fresh: true}), want)
}

func TestRoundTripRelay(t *testing.T) {
	f := vcstest.New(t)
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv-b")))
	rt.commit(rt.src, "feature.txt", "unpushed\n")
	f.WriteFile(rt.src, "README.md", "staged\n")
	f.RunGit(rt.src, "add", "README.md")
	f.WriteFile(rt.src, "README.md", "unstaged\n")
	f.WriteFile(rt.src, "notes.txt", "untracked\n")
	snap, want := rt.tick()
	enc, err := worktree.Encode(snap)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := rt.art.Put(t.Context(), worktree.MediaSnapshot, bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	refs := append(snap.Artifacts(), manifest)

	onB := worktreetest.New()
	if err := rt.art.CopyTo(onB, refs); err != nil {
		t.Fatal(err)
	}
	rt.assertRestored(snap, rt.pickup(rt.store, rt.recv, onB, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "on-b")}), want)

	onC := worktreetest.New()
	if err := onB.CopyTo(onC, refs); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{rt.src, filepath.Join(f.Root, "source-store")} {
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
	}
	storeC, err := worktree.OpenStore(filepath.Join(f.Root, "store-c"))
	if err != nil {
		t.Fatal(err)
	}
	relayed, err := worktree.Decode([]byte(readArtifact(t, onC, manifest)))
	if err != nil {
		t.Fatal(err)
	}
	recvC := f.GitClone(filepath.Join(f.Root, "recv-c"))
	rt.assertRestored(relayed, rt.pickup(storeC, recvC, onC, relayed, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "on-c")}), want)
}
