package worktree_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/registry"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

const testOrigin = "https://example.com/repo.git"

func discoverAt(t *testing.T, repo, root string) worktree.Worktree {
	t.Helper()
	reg := registry.Registry{Repos: []registry.Repo{{Relpath: "repo", Path: repo, Origin: testOrigin, Trunk: "main"}}}
	wts, _, err := worktree.Discover(context.Background(), reg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	wt, ok := worktree.Locate(wts, root)
	if !ok || wt.Root != root {
		t.Fatalf("no worktree at %s in %+v", root, wts)
	}
	return wt
}

func openStore(t *testing.T) *worktree.Store {
	t.Helper()
	st, err := worktree.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return st
}

func capture(t *testing.T, st *worktree.Store, wt worktree.Worktree, sink worktree.ArtifactSink, limits worktree.Limits) (worktree.Snapshot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	snap, err := st.Capture(ctx, wt, sink, worktree.CaptureOptions{Source: "host-a", Limits: limits})
	if err == nil {
		if _, encErr := worktree.Encode(snap); encErr != nil {
			t.Fatalf("Encode(captured snapshot): %v", encErr)
		}
	}
	return snap, err
}

func mustCapture(t *testing.T, st *worktree.Store, wt worktree.Worktree, sink worktree.ArtifactSink) worktree.Snapshot {
	t.Helper()
	snap, err := capture(t, st, wt, sink, worktree.Limits{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return snap
}

func trees(f *vcstest.Fixture, dirs ...string) map[string]vcstest.TreeEntry {
	all := map[string]vcstest.TreeEntry{}
	for _, d := range dirs {
		for p, e := range f.SnapshotTree(d) {
			all[d+"//"+p] = e
		}
	}
	return all
}

func filePaths(snap worktree.Snapshot) []string {
	paths := make([]string, 0, len(snap.Files))
	for _, e := range snap.Files {
		paths = append(paths, string(e.Kind)+":"+e.Path)
	}
	return paths
}

func readArtifact(t *testing.T, src worktree.ArtifactSource, ref worktree.ArtifactRef) string {
	t.Helper()
	r, err := src.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("Open(%v): %v", ref, err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %v: %v", ref, err)
	}
	return string(b)
}

func TestCaptureReadOnly(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		setup func(t *testing.T, f *vcstest.Fixture) (repo, root string, dirs []string)
	}{
		{"git", []string{"file:README.md", "file:dir/untracked.txt"}, func(t *testing.T, f *vcstest.Fixture) (string, string, []string) {
			repo := f.GitClone(filepath.Join(f.Root, "repo"))
			f.WriteFile(repo, "c.txt", "committed\n")
			f.RunGit(repo, "add", "c.txt")
			f.RunGit(repo, "commit", "-qm", "ahead")
			f.WriteFile(repo, "README.md", "staged\n")
			f.RunGit(repo, "add", "README.md")
			f.WriteFile(repo, "README.md", "unstaged\n")
			if err := os.Mkdir(filepath.Join(repo, "dir"), 0o750); err != nil {
				t.Fatal(err)
			}
			f.WriteFile(repo, "dir/untracked.txt", "new\n")
			return repo, repo, []string{filepath.Join(repo, ".git")}
		}},
		{"linked", []string{"file:README.md"}, func(_ *testing.T, f *vcstest.Fixture) (string, string, []string) {
			repo := f.GitClone(filepath.Join(f.Root, "repo"))
			linked := f.LinkedWorktree(repo, filepath.Join(f.Root, "linked"), "feat")
			f.WriteFile(linked, "README.md", "linked edit\n")
			return repo, linked, []string{filepath.Join(repo, ".git")}
		}},
		{"jj-colocated", []string{"file:README.md"}, func(_ *testing.T, f *vcstest.Fixture) (string, string, []string) {
			repo := f.JJClone(filepath.Join(f.Root, "repo"))
			f.WriteFile(repo, "c.txt", "committed\n")
			f.RunJJ(repo, "commit", "-m", "ahead")
			f.WriteFile(repo, "README.md", "unsnapshotted\n")
			return repo, repo, []string{filepath.Join(repo, ".git"), filepath.Join(repo, ".jj")}
		}},
		{"jj-workspace", []string{"file:README.md", "file:new.txt"}, func(_ *testing.T, f *vcstest.Fixture) (string, string, []string) {
			repo := f.JJClone(filepath.Join(f.Root, "repo"))
			ws := f.JJWorkspace(repo, filepath.Join(f.Root, "ws"), "second")
			f.WriteFile(ws, "README.md", "workspace edit\n")
			f.WriteFile(ws, "new.txt", "workspace new\n")
			return repo, ws, []string{filepath.Join(repo, ".git"), filepath.Join(repo, ".jj"), filepath.Join(ws, ".jj")}
		}},
		{"lfs-and-filter", []string{"file:base.bin", "file:x.fake"}, func(t *testing.T, f *vcstest.Fixture) (string, string, []string) {
			f.EnableLFS("*.bin")
			f.WriteFile(f.Seed, "base.bin", "base asset\x00\x01")
			f.RunGit(f.Seed, "add", "base.bin")
			f.RunGit(f.Seed, "commit", "-qm", "asset")
			f.RunGit(f.Seed, "push", "-q", "origin", "main")
			repo := f.LFSClone(filepath.Join(f.Root, "repo"))
			sentinel := filepath.Join(f.Root, "sentinel")
			f.RunGit(repo, "config", "filter.fake.clean", "sh -c 'touch "+sentinel+"; cat'")
			f.RunGit(repo, "config", "filter.fake.smudge", "cat")
			f.WriteFile(repo, ".gitattributes", f.ReadFile(repo, ".gitattributes")+"*.fake filter=fake\n")
			f.WriteFile(repo, "x.fake", "fake v1\n")
			f.RunGit(repo, "add", ".gitattributes", "x.fake")
			f.RunGit(repo, "commit", "-qm", "fake")
			f.WriteFile(repo, "x.fake", "fake v2\n")
			f.WriteFile(repo, "base.bin", "edited asset\x00\x02")
			f.WriteFile(repo, "staged.bin", "staged asset\x00\x03")
			f.RunGit(repo, "add", "staged.bin")
			if err := os.Remove(sentinel); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := os.Stat(sentinel); err == nil {
					t.Error("a clean filter ran during capture")
				}
			})
			return repo, repo, []string{filepath.Join(repo, ".git")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			repo, root, dirs := tt.setup(t, f)
			wt := discoverAt(t, repo, root)
			before := trees(f, dirs...)
			snap := mustCapture(t, openStore(t), wt, worktreetest.New())
			if after := trees(f, dirs...); !reflect.DeepEqual(before, after) {
				for p, e := range after {
					if before[p] != e {
						t.Errorf("source changed: %s", p)
					}
				}
				for p := range before {
					if _, ok := after[p]; !ok {
						t.Errorf("source lost: %s", p)
					}
				}
			}
			if got := filePaths(snap); !snap.Complete || !slices.Equal(got, tt.files) {
				t.Errorf("snapshot complete=%v files=%v, want complete with %v", snap.Complete, got, tt.files)
			}
		})
	}
}

func TestCaptureDeferral(t *testing.T) {
	var deferred *worktree.DeferredError
	tests := []struct {
		name  string
		jj    bool
		setup func(t *testing.T, f *vcstest.Fixture, repo string, sink *worktreetest.Store)
		want  func(error) bool
	}{
		{"merge", false, marker("MERGE_HEAD", false), isDeferred},
		{"rebase-merge", false, marker("rebase-merge", true), isDeferred},
		{"rebase-apply", false, marker("rebase-apply", true), isDeferred},
		{"cherry-pick", false, marker("CHERRY_PICK_HEAD", false), isDeferred},
		{"revert", false, marker("REVERT_HEAD", false), isDeferred},
		{"bisect", false, marker("BISECT_LOG", false), isDeferred},
		{"sequencer", false, marker("sequencer", true), isDeferred},
		{"index-lock", false, marker("index.lock", false), isBusy},
		{"unmerged", false, func(t *testing.T, f *vcstest.Fixture, repo string, _ *worktreetest.Store) {
			f.RunGit(repo, "checkout", "-qb", "other")
			f.WriteFile(repo, "README.md", "other\n")
			f.RunGit(repo, "commit", "-qam", "other")
			f.RunGit(repo, "checkout", "-q", "main")
			f.WriteFile(repo, "README.md", "mine\n")
			f.RunGit(repo, "commit", "-qam", "mine")
			//nolint:gosec // G204: git against a test-controlled temp repo; the merge must fail.
			if err := exec.Command("git", "-C", repo, "merge", "-q", "other").Run(); err == nil {
				t.Fatal("merge unexpectedly succeeded")
			}
			for _, m := range []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE"} {
				_ = os.Remove(filepath.Join(repo, ".git", m))
			}
		}, isDeferred},
		{"jj-conflict", true, func(_ *testing.T, f *vcstest.Fixture, repo string, _ *worktreetest.Store) {
			f.WriteFile(repo, "README.md", "x\n")
			f.RunJJ(repo, "commit", "-m", "x")
			x := strings.TrimSpace(f.RunJJ(repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"))
			f.RunJJ(repo, "new", "main")
			f.WriteFile(repo, "README.md", "y\n")
			f.RunJJ(repo, "commit", "-m", "y")
			y := strings.TrimSpace(f.RunJJ(repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"))
			f.RunJJ(repo, "new", x, y)
		}, isDeferred},
		{"jj-working-copy-lock", true, func(_ *testing.T, f *vcstest.Fixture, repo string, _ *worktreetest.Store) {
			f.WriteFile(repo, ".jj/working_copy/working_copy.lock", "")
		}, isBusy},
		{"head-moved", false, func(_ *testing.T, f *vcstest.Fixture, repo string, sink *worktreetest.Store) {
			f.WriteFile(repo, "dirty.txt", "dirty\n")
			moved := false
			sink.OnPut = func(worktree.ArtifactRef) {
				if !moved {
					moved = true
					f.RunGit(repo, "commit", "--allow-empty", "-qm", "moved")
				}
			}
		}, isBusy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			repo := filepath.Join(f.Root, "repo")
			if tt.jj {
				f.JJClone(repo)
			} else {
				f.GitClone(repo)
			}
			sink := worktreetest.New()
			tt.setup(t, f, repo, sink)
			_, err := capture(t, openStore(t), discoverAt(t, repo, repo), sink, worktree.Limits{})
			if !tt.want(err) {
				t.Fatalf("Capture error = %v (deferred=%v busy=%v)", err, errors.As(err, &deferred), errors.Is(err, worktree.ErrBusy))
			}
		})
	}
}

func marker(name string, dir bool) func(*testing.T, *vcstest.Fixture, string, *worktreetest.Store) {
	return func(t *testing.T, _ *vcstest.Fixture, repo string, _ *worktreetest.Store) {
		path := filepath.Join(repo, ".git", name)
		var err error
		if dir {
			err = os.Mkdir(path, 0o750)
		} else {
			err = os.WriteFile(path, nil, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func isDeferred(err error) bool {
	var d *worktree.DeferredError
	return errors.As(err, &d)
}

func isBusy(err error) bool {
	return errors.Is(err, worktree.ErrBusy) && !isDeferred(err)
}

func TestCaptureIncremental(t *testing.T) {
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	trunk := strings.TrimSpace(f.RunGit(repo, "rev-parse", "origin/main"))
	commit := func(name string) string {
		f.WriteFile(repo, name, name+"\n")
		f.RunGit(repo, "add", name)
		f.RunGit(repo, "commit", "-qm", name)
		return strings.TrimSpace(f.RunGit(repo, "rev-parse", "HEAD"))
	}
	commit("c1.txt")
	f.WriteFile(repo, "README.md", "edit 1\n")
	f.WriteFile(repo, "big.bin", strings.Repeat("x", 1<<20))
	wt := discoverAt(t, repo, repo)
	storeRoot := t.TempDir()
	st, err := worktree.OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	sink := worktreetest.New()

	first := mustCapture(t, st, wt, sink)
	if len(first.History) != 1 || !slices.Equal(first.Requires, []string{trunk}) {
		t.Fatalf("first history %+v requires %v, want one link requiring %s", first.History, first.Requires, trunk)
	}

	sink.ResetPuts()
	if again := mustCapture(t, st, wt, sink); again.Digest != first.Digest || len(sink.Puts()) != 0 {
		t.Fatalf("unchanged recapture: digest %s vs %s, puts %v", again.Digest, first.Digest, sink.Puts())
	}

	stand, err := sink.Put(context.Background(), worktree.MediaFile, strings.NewReader("stand-in"))
	if err != nil {
		t.Fatal(err)
	}
	patchLedger(t, storeRoot, wt.ID, "big.bin", stand)
	sink.ResetPuts()
	cached := mustCapture(t, st, wt, sink)
	if i := slices.IndexFunc(cached.Files, func(e worktree.FileEntry) bool { return e.Path == "big.bin" }); i < 0 || *cached.Files[i].Content != stand || len(sink.Puts()) != 0 {
		t.Fatalf("stat-cached big.bin was re-read: files %+v puts %v", cached.Files, sink.Puts())
	}
	patchLedger(t, storeRoot, wt.ID, "big.bin", *first.Files[slices.IndexFunc(first.Files, func(e worktree.FileEntry) bool { return e.Path == "big.bin" })].Content)

	f.WriteFile(repo, "README.md", "edit 2\n")
	edited := mustCapture(t, st, wt, sink)
	if puts := sink.Puts(); len(puts) != 1 || puts[0].Media != worktree.MediaFile {
		t.Fatalf("one edited file: puts %v, want exactly one file put", puts)
	}

	sink.ResetPuts()
	c2 := commit("c2.txt")
	appended := mustCapture(t, st, wt, sink)
	if len(appended.History) != 2 || !reflect.DeepEqual(appended.History[0], edited.History[0]) || appended.History[1].Tip != c2 || sink.PutsOf(worktree.MediaBundle) != 1 {
		t.Fatalf("new commit: history %+v, bundle puts %d; want the old link plus one new", appended.History, sink.PutsOf(worktree.MediaBundle))
	}
	if !slices.Equal(appended.Requires, []string{trunk}) {
		t.Fatalf("appended requires %v, want [%s]", appended.Requires, trunk)
	}

	f.RunGit(repo, "commit", "--amend", "-qm", "c2 amended")
	amended := mustCapture(t, st, wt, sink)
	if len(amended.History) != 1 || !slices.Equal(amended.History[0].Prerequisites, []string{trunk}) {
		t.Fatalf("amend: history %+v, want a new one-link chain from trunk", amended.History)
	}

	limits := worktree.Limits{MaxChainLinks: 2}
	commit("c3.txt")
	if snap, err := capture(t, st, wt, sink, limits); err != nil || len(snap.History) != 2 {
		t.Fatalf("c3: history %d links, err %v; want 2", len(snap.History), err)
	}
	c4 := commit("c4.txt")
	snap, err := capture(t, st, wt, sink, limits)
	if err != nil || len(snap.History) != 1 || snap.History[0].Tip != c4 || snap.Head.Ahead != 4 {
		t.Fatalf("chain at MaxChainLinks: history %+v ahead %d err %v; want a new one-link chain", snap.History, snap.Head.Ahead, err)
	}
}

func patchLedger(t *testing.T, storeRoot, worktreeID, path string, ref worktree.ArtifactRef) {
	t.Helper()
	ledgerPath := filepath.Join(storeRoot, "source", worktreeID+".reposync-worktree-ledger-v2.json")
	//nolint:gosec // G304: the ledger under a test-controlled store root.
	b, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&l); err != nil {
		t.Fatal(err)
	}
	entry := l["files"].(map[string]any)[path].(map[string]any)
	entry["content"] = map[string]any{"digest": ref.Digest, "size": ref.Size, "media": ref.Media}
	if b, err = json.Marshal(l); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureBudgets(t *testing.T) {
	tests := []struct {
		name   string
		limits worktree.Limits
		reason worktree.PartialReason
	}{
		{"entries", worktree.Limits{MaxEntries: 1}, worktree.PartialEntries},
		{"new-bytes", worktree.Limits{MaxNewBytes: 10}, worktree.PartialNewBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			repo := f.GitClone(filepath.Join(f.Root, "repo"))
			for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
				f.WriteFile(repo, n, n+"-data")
			}
			wt := discoverAt(t, repo, repo)
			st, sink := openStore(t), worktreetest.New()
			var remaining [][]string
			var snap worktree.Snapshot
			for range 5 {
				var err error
				snap, err = capture(t, st, wt, sink, tt.limits)
				var partial *worktree.PartialError
				if errors.As(err, &partial) {
					if partial.Reason != tt.reason {
						t.Fatalf("partial reason %q, want %q", partial.Reason, tt.reason)
					}
					remaining = append(remaining, partial.Remaining)
					continue
				}
				if err != nil {
					t.Fatalf("Capture: %v", err)
				}
				break
			}
			want := [][]string{{"b.txt", "c.txt"}, {"c.txt"}}
			if !reflect.DeepEqual(remaining, want) {
				t.Fatalf("partial remaining %v, want %v", remaining, want)
			}
			if got := filePaths(snap); !slices.Equal(got, []string{"file:a.txt", "file:b.txt", "file:c.txt"}) || !snap.Complete {
				t.Fatalf("resumed snapshot files %v complete=%v", got, snap.Complete)
			}
			if puts := sink.PutsOf(worktree.MediaFile); puts != 3 {
				t.Fatalf("file puts %d across resumes, want 3", puts)
			}
		})
	}
}

func TestCaptureOmissions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *vcstest.Fixture, repo string)
		want  []worktree.Omission
	}{
		{"dirty-submodule", func(_ *testing.T, f *vcstest.Fixture, repo string) {
			f.RunGit(repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", f.Origin, "sub")
			f.RunGit(repo, "commit", "-qm", "submodule")
			f.WriteFile(filepath.Join(repo, "sub"), "README.md", "dirty inside\n")
		}, []worktree.Omission{{Path: "sub", Reason: worktree.OmitSubmodule}}},
		{"nested-repo", func(_ *testing.T, f *vcstest.Fixture, repo string) {
			f.RunGit(repo, "init", "-q", filepath.Join(repo, "nested"))
			f.WriteFile(filepath.Join(repo, "nested"), "x.txt", "x\n")
		}, []worktree.Omission{{Path: "nested", Reason: worktree.OmitNestedRepo}}},
		{"fifo", func(t *testing.T, _ *vcstest.Fixture, repo string) {
			if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(repo, "README.md"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []worktree.Omission{{Path: "README.md", Reason: worktree.OmitSpecialFile}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			repo := f.GitClone(filepath.Join(f.Root, "repo"))
			tt.setup(t, f, repo)
			f.WriteFile(repo, "wip.txt", "wip\n")
			snap := mustCapture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New())
			if snap.Complete || !reflect.DeepEqual(snap.Omitted, tt.want) {
				t.Fatalf("complete=%v omitted=%+v, want incomplete with %+v", snap.Complete, snap.Omitted, tt.want)
			}
			if got := filePaths(snap); !slices.Contains(got, "file:wip.txt") {
				t.Fatalf("files %v lack the capturable wip.txt", got)
			}
		})
	}
}

func TestCaptureLFS(t *testing.T) {
	base := "base asset\x00\x01"
	type env struct {
		f      *vcstest.Fixture
		repo   string
		common string
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, e env)
		check func(t *testing.T, e env, snap worktree.Snapshot, sink *worktreetest.Store, err error)
	}{
		{
			"unchanged-lfs-repo-is-complete", func(*testing.T, env) {},
			func(t *testing.T, e env, snap worktree.Snapshot, _ *worktreetest.Store, err error) {
				if err != nil || !snap.Complete || len(snap.Files) != 0 || len(snap.LFSObjects) != 0 || snap.LFS == nil {
					t.Fatalf("err %v complete=%v files=%v lfs=%+v objects=%v", err, snap.Complete, filePaths(snap), snap.LFS, snap.LFSObjects)
				}
				if snap.LFS.Remote != "file://"+e.f.Origin {
					t.Fatalf("lfs remote %q, want file://%s", snap.LFS.Remote, e.f.Origin)
				}
			},
		},
		{"modified-lfs-file-ships-raw-bytes", func(_ *testing.T, e env) {
			e.f.WriteFile(e.repo, "base.bin", "edited\x00\x02")
		}, func(t *testing.T, _ env, snap worktree.Snapshot, sink *worktreetest.Store, err error) {
			if err != nil || len(snap.Files) != 1 || snap.Files[0].Path != "base.bin" || len(snap.LFSObjects) != 0 {
				t.Fatalf("err %v files %v objects %v", err, filePaths(snap), snap.LFSObjects)
			}
			if got := readArtifact(t, sink, *snap.Files[0].Content); got != "edited\x00\x02" {
				t.Fatalf("shipped %q, want the raw edited bytes", got)
			}
		}},
		{"staged-lfs-file-ships-pointer-and-object", func(_ *testing.T, e env) {
			e.f.WriteFile(e.repo, "new.bin", "staged\x00\x03")
			e.f.RunGit(e.repo, "add", "new.bin")
		}, func(t *testing.T, _ env, snap worktree.Snapshot, sink *worktreetest.Store, err error) {
			oid := vcstest.SHA256([]byte("staged\x00\x03"))
			if err != nil || len(snap.Index) != 1 || snap.Index[0].Path != "new.bin" || len(snap.Files) != 0 {
				t.Fatalf("err %v index %+v files %v", err, snap.Index, filePaths(snap))
			}
			if ptr := readArtifact(t, sink, *snap.Index[0].Blob); !strings.Contains(ptr, "oid sha256:"+oid) {
				t.Fatalf("staged blob %q is not the pointer", ptr)
			}
			wantRefs := []worktree.LFSObjectRef{{Path: "new.bin", OID: oid, Size: 8}}
			if !reflect.DeepEqual(snap.LFS.Objects, wantRefs) || len(snap.LFSObjects) != 1 || snap.LFSObjects[0].OID != oid {
				t.Fatalf("lfs %+v objects %+v, want %+v", snap.LFS.Objects, snap.LFSObjects, wantRefs)
			}
			if got := readArtifact(t, sink, snap.LFSObjects[0].Artifact); got != "staged\x00\x03" {
				t.Fatalf("lfs object %q", got)
			}
		}},
		{"unpushed-commit-ships-lfs-object", func(_ *testing.T, e env) {
			e.f.WriteFile(e.repo, "hist.bin", "history\x00\x04")
			e.f.RunGit(e.repo, "add", "hist.bin")
			e.f.RunGit(e.repo, "commit", "-qm", "hist")
		}, func(t *testing.T, _ env, snap worktree.Snapshot, sink *worktreetest.Store, err error) {
			oid := vcstest.SHA256([]byte("history\x00\x04"))
			if err != nil || len(snap.History) != 1 || len(snap.LFSObjects) != 1 || snap.LFSObjects[0].OID != oid {
				t.Fatalf("err %v history %d objects %+v", err, len(snap.History), snap.LFSObjects)
			}
			if !reflect.DeepEqual(snap.LFS.Objects, []worktree.LFSObjectRef{{Path: "hist.bin", OID: oid, Size: 9}}) {
				t.Fatalf("lfs refs %+v", snap.LFS.Objects)
			}
			if got := readArtifact(t, sink, snap.LFSObjects[0].Artifact); got != "history\x00\x04" {
				t.Fatalf("lfs object %q", got)
			}
		}},
		{"stat-touched-lfs-file-is-not-wip", func(t *testing.T, e env) {
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(e.repo, "base.bin"), future, future); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, _ env, snap worktree.Snapshot, sink *worktreetest.Store, err error) {
			if err != nil || !snap.Complete || len(snap.Files) != 0 || len(sink.Puts()) != 0 {
				t.Fatalf("err %v files %v puts %v", err, filePaths(snap), sink.Puts())
			}
		}},
		{"missing-lfs-object", func(t *testing.T, e env) {
			e.f.WriteFile(e.repo, "gone.bin", "gone\x00\x05")
			e.f.RunGit(e.repo, "add", "gone.bin")
			if err := os.Remove(vcstest.LFSObjectPath(e.common, vcstest.SHA256([]byte("gone\x00\x05")))); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, _ env, _ worktree.Snapshot, _ *worktreetest.Store, err error) {
			var missing *worktree.MissingLFSError
			want := []worktree.LFSObjectRef{{Path: "gone.bin", OID: vcstest.SHA256([]byte("gone\x00\x05")), Size: 6}}
			if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Objects, want) {
				t.Fatalf("Capture error %v, want MissingLFSError %+v", err, want)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			f.EnableLFS("*.bin")
			f.WriteFile(f.Seed, "base.bin", base)
			f.RunGit(f.Seed, "add", "base.bin")
			f.RunGit(f.Seed, "commit", "-qm", "asset")
			f.RunGit(f.Seed, "push", "-q", "origin", "main")
			repo := f.LFSClone(filepath.Join(f.Root, "repo"))
			e := env{f: f, repo: repo, common: filepath.Join(repo, ".git")}
			tt.setup(t, e)
			wt := discoverAt(t, repo, repo)
			lfsBefore := f.SnapshotTree(filepath.Join(e.common, "lfs"))
			gitBefore := f.SnapshotTree(e.common)
			sink := worktreetest.New()
			snap, err := capture(t, openStore(t), wt, sink, worktree.Limits{})
			if !reflect.DeepEqual(lfsBefore, f.SnapshotTree(filepath.Join(e.common, "lfs"))) || !reflect.DeepEqual(gitBefore, f.SnapshotTree(e.common)) {
				t.Fatal("capture changed the source .git or .git/lfs")
			}
			tt.check(t, e, snap, sink, err)
		})
	}
}
