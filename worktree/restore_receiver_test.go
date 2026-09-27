package worktree_test

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
)

func TestRestoreIgnoredIntentToAdd(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "new.tmp", "tracked by intent\n")
	h.f.RunGit(h.src, "add", "-N", "new.tmp")
	h.f.WriteFile(h.src, ".gitignore", "*.tmp\n")
	snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
	if !snap.Complete || !slices.Equal(snap.IntentToAdd, []worktree.IntentToAdd{{Path: "new.tmp", Mode: "100644"}}) {
		t.Fatalf("capture: complete %t, intent-to-add %q", snap.Complete, snap.IntentToAdd)
	}
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "recovered")}))
}

func TestRestoreSparseOutsideConeIntentToAdd(t *testing.T) {
	f := vcstest.New(t)
	src := sparseSource(t, f, false, "kept")
	writeBytes(t, src, "outside/new.txt", []byte("outside the cone\n"))
	f.RunGit(src, "add", "--sparse", "-N", "outside/new.txt")
	rt := newRoundTrip(t, f, src, f.GitClone(filepath.Join(f.Root, "recv")))
	snap, want := rt.tick()
	if snap.Sparse == nil || !slices.Equal(snap.IntentToAdd, []worktree.IntentToAdd{{Path: "outside/new.txt", Mode: "100644"}}) || !slices.Equal(snap.Sparse.Exceptions, []string{"outside/new.txt"}) {
		t.Fatalf("capture: sparse %+v, intent-to-add %q", snap.Sparse, snap.IntentToAdd)
	}
	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered"), ApplySparse: true})
	if !r.Exact || len(r.Differences) != 0 {
		t.Fatalf("restored %+v, want exact", r)
	}
	if got := rt.worktreeFiles(r.Path); !maps.Equal(want.files, got) {
		t.Fatalf("worktree differs at %q", changed(want.files, got))
	}
	if got := rt.status(r.Path); !slices.Equal(want.status, got) {
		t.Fatalf("status differs:\nsource   %q\nrestored %q", want.status, got)
	}
}

func TestRestoreApplySparseSkipsReceiverFSMonitor(t *testing.T) {
	f := vcstest.New(t)
	src := sparseSource(t, f, false, "kept")
	rt := newRoundTrip(t, f, src, f.GitClone(filepath.Join(f.Root, "recv")))
	rt.f.WriteFile(rt.src, "kept/inside.txt", "edited inside\n")
	snap, _ := rt.tick()
	rt.f.RunGit(rt.recv, "config", "core.fsmonitor", sentinelScript(t, filepath.Join(f.Root, "fsmonitor.sh")))
	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered"), ApplySparse: true})
	if !r.Exact {
		t.Fatalf("restored %+v, want exact", r)
	}
}

func newUnreachableSubmoduleHarness(t *testing.T) (*harness, *atomic.Int32) {
	t.Helper()
	f := vcstest.New(t)
	sub := filepath.Join(f.Root, "sub-origin")
	f.RunGit(f.Root, "init", "-q", "-b", "main", sub)
	f.ConfigGit(sub)
	f.WriteFile(sub, "sub.txt", "content\n")
	f.RunGit(sub, "add", ".")
	f.RunGit(sub, "commit", "-qm", "sub init")
	f.RunGit(f.Seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	f.RunGit(f.Seed, "commit", "-qm", "add sub")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	h := newHarness(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
	f.RunGit(h.recv, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	f.RunGit(filepath.Join(h.recv, "sub"), "remote", "set-url", "origin", server.URL+"/sub.git")
	f.RunGit(h.recv, "config", "fetch.recurseSubmodules", "true")
	return h, &requests
}

func TestRestoreFetchesNoSubmoduleRemote(t *testing.T) {
	h, requests := newUnreachableSubmoduleHarness(t)
	h.f.WriteFile(h.src, "README.md", "work in progress\n")
	snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
	r := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "recovered")})
	if n := requests.Load(); n != 0 || !r.Exact {
		t.Fatalf("restore made %d network requests, restored %+v", n, r)
	}
}

func TestRestoreLeavesLegacySparseConfig(t *testing.T) {
	h := newGitHarness(t)
	h.f.RunGit(h.recv, "config", "core.sparseCheckout", "true")
	h.f.WriteFile(h.recv, ".git/info/sparse-checkout", "/*\n")
	h.f.WriteFile(h.src, "README.md", "edited\n")
	snap := h.seal(h.capture())
	before := h.f.ReadFile(h.recv, ".git/config")
	r := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "recovered")})
	if after := h.f.ReadFile(h.recv, ".git/config"); after != before {
		t.Fatalf("receiver config changed:\nbefore %s\nafter  %s", before, after)
	}
	h.assertFaithful(r)
}

func TestRestoreIntentToAddIndexModes(t *testing.T) {
	h := newGitHarness(t)
	src := func(p string) string { return filepath.Join(h.src, p) }
	h.f.WriteFile(h.src, "run.sh", "#!/bin/sh\n")
	h.f.WriteFile(h.src, "retyped", "plain\n")
	h.f.WriteFile(h.src, "chmodded", "plain\n")
	for _, step := range []func() error{
		//nolint:gosec // G302: the fixture needs an executable intent-to-add entry.
		func() error { return os.Chmod(src("run.sh"), 0o755) },
		func() error { return os.Symlink("README.md", src("link")) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	h.f.RunGit(h.src, "rm", "-q", "--cached", "README.md")
	h.f.RunGit(h.src, "add", "-N", "run.sh", "link", "retyped", "chmodded", "README.md")
	for _, step := range []func() error{
		func() error { return os.Remove(src("run.sh")) },
		func() error { return os.Remove(src("link")) },
		func() error { return os.Remove(src("README.md")) },
		func() error { return os.Remove(src("retyped")) },
		func() error { return os.Symlink("chmodded", src("retyped")) },
		//nolint:gosec // G302: the fixture changes an intent-to-add file's mode after add -N.
		func() error { return os.Chmod(src("chmodded"), 0o755) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
	want := []worktree.IntentToAdd{
		{Path: "README.md", Mode: "100644"},
		{Path: "chmodded", Mode: "100644"},
		{Path: "link", Mode: "120000"},
		{Path: "retyped", Mode: "100644"},
		{Path: "run.sh", Mode: "100755"},
	}
	if !snap.Complete || !slices.Equal(snap.IntentToAdd, want) {
		t.Fatalf("capture: complete %t, intent-to-add %+v", snap.Complete, snap.IntentToAdd)
	}
	r := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "recovered")})
	h.assertFaithful(r)
	if got, want := h.git(r.Path, "ls-files", "-s", "--debug"), h.git(h.src, "ls-files", "-s", "--debug"); stripStat(got) != stripStat(want) {
		t.Fatalf("index differs:\nsource   %s\nrestored %s", want, got)
	}
}

func TestRestoreSparseHiddenDeletedIntentToAdd(t *testing.T) {
	f := vcstest.New(t)
	src := sparseSource(t, f, false, "kept")
	writeBytes(t, src, "ita", []byte("intent\n"))
	f.RunGit(src, "add", "-N", "ita")
	f.RunGit(src, "update-index", "--skip-worktree", "ita")
	if err := os.Remove(filepath.Join(src, "ita")); err != nil {
		t.Fatal(err)
	}
	rt := newRoundTrip(t, f, src, f.GitClone(filepath.Join(f.Root, "recv")))
	snap, want := rt.tick()
	if !slices.Equal(snap.IntentToAdd, []worktree.IntentToAdd{{Path: "ita", Mode: "100644"}}) {
		t.Fatalf("capture: intent-to-add %+v", snap.IntentToAdd)
	}
	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered"), ApplySparse: true})
	if !r.Exact || len(r.Differences) != 0 {
		t.Fatalf("restored %+v, want exact", r)
	}
	if got := rt.worktreeFiles(r.Path); !maps.Equal(want.files, got) {
		t.Fatalf("worktree differs at %q", changed(want.files, got))
	}
	if got, want := rt.git(r.Path, "ls-files", "-s", "-t", "--debug"), rt.git(src, "ls-files", "-s", "-t", "--debug"); stripStat(got) != stripStat(want) {
		t.Fatalf("index differs:\nsource   %s\nrestored %s", want, got)
	}
}

func stripStat(debug string) string {
	var keep []string
	for l := range strings.SplitSeq(debug, "\n") {
		if f := strings.Fields(l); len(f) == 0 || !slices.Contains([]string{"ctime:", "mtime:", "dev:", "uid:", "size:"}, f[0]) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}
