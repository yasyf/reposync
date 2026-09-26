package worktree_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCaptureSubmoduleReadOnly(t *testing.T) {
	f := vcstest.New(t)
	sub := filepath.Join(f.Root, "subrepo")
	f.RunGit(f.Root, "init", "-q", "-b", "main", sub)
	f.ConfigGit(sub)
	f.WriteFile(sub, ".gitattributes", "*.dat filter=subspy\n")
	f.WriteFile(sub, "x.dat", "data\n")
	f.RunGit(sub, "add", "-A")
	f.RunGit(sub, "commit", "-q", "-m", "sub")
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	f.RunGit(repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	f.RunGit(repo, "commit", "-q", "-m", "add sub")
	inner := filepath.Join(repo, "sub")
	f.RunGit(inner, "config", "filter.subspy.clean", "touch '"+filepath.Join(f.Root, "sub-filter-ran")+"'; cat")
	f.RunGit(inner, "config", "filter.subspy.required", "true")
	f.RunGit(inner, "config", "core.fsmonitor", fsmonitorHook(t, f, "sub-fsmonitor-ran"))
	f.RunGit(repo, "config", "core.fsmonitor", fsmonitorHook(t, f, "fsmonitor-ran"))
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(inner, "x.dat"), future, future); err != nil {
		t.Fatal(err)
	}
	f.WriteFile(repo, "README.md", "outer edit\n")
	sentinels := []string{"sub-filter-ran", "sub-fsmonitor-ran", "fsmonitor-ran"}

	snap := mustCapture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New())
	for _, ran := range sentinels {
		if f.FileExists(f.Root, ran) {
			t.Fatalf("Capture left %s", ran)
		}
	}
	if got := filePaths(snap); !snap.Complete || !slices.Equal(got, []string{"file:README.md"}) {
		t.Fatalf("snapshot complete=%v files=%v, want complete with README.md", snap.Complete, got)
	}
	f.RunGit(repo, "status")
	for _, ran := range sentinels {
		if !f.FileExists(f.Root, ran) {
			t.Fatalf("a plain git status never left %s; the fixture proves nothing", ran)
		}
	}
}

func TestRoundTripHiddenFlags(t *testing.T) {
	f := vcstest.New(t)
	for _, p := range []string{"au.txt", "sw.txt", "au-del.txt", "sw-same.txt", "au-chmod.sh"} {
		f.WriteFile(f.Seed, p, p+" base\n")
	}
	f.RunGit(f.Seed, "add", "-A")
	f.RunGit(f.Seed, "commit", "-qm", "flagged base")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
	f.RunGit(rt.src, "update-index", "--assume-unchanged", "au.txt", "au-del.txt", "au-chmod.sh")
	f.RunGit(rt.src, "update-index", "--skip-worktree", "sw.txt", "sw-same.txt")
	f.WriteFile(rt.src, "au.txt", "assume-unchanged edit\n")
	f.WriteFile(rt.src, "sw.txt", "skip-worktree edit\n")
	if err := os.Remove(filepath.Join(rt.src, "au-del.txt")); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G302: the fixture needs an executable file to prove a hidden exec-bit change round-trips.
	if err := os.Chmod(filepath.Join(rt.src, "au-chmod.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st := rt.status(rt.src); len(st) != 0 {
		t.Fatalf("git status shows %q; the flags hide nothing", st)
	}

	snap, want := rt.tick()
	for path, e := range map[string]worktree.FileEntry{
		"au.txt":      {Kind: worktree.FileRegular, AssumeUnchanged: true},
		"sw.txt":      {Kind: worktree.FileRegular, SkipWorktree: true},
		"au-del.txt":  {Kind: worktree.FileDeleted, AssumeUnchanged: true},
		"au-chmod.sh": {Kind: worktree.FileRegular, Executable: true, AssumeUnchanged: true},
	} {
		got, ok := fileEntry(snap, path)
		if !ok || got.Kind != e.Kind || got.Executable != e.Executable || got.AssumeUnchanged != e.AssumeUnchanged || got.SkipWorktree != e.SkipWorktree || got.Untracked {
			t.Fatalf("%s captured as %+v, want %+v", path, got, e)
		}
	}
	if _, ok := fileEntry(snap, "sw-same.txt"); ok || len(snap.Files) != 4 || !snap.Complete {
		t.Fatalf("snapshot files %v complete=%v, want exactly the four hidden edits", filePaths(snap), snap.Complete)
	}
	if got := readArtifact(t, rt.art, *mustFileEntry(t, snap, "au.txt").Content); got != "assume-unchanged edit\n" {
		t.Fatalf("au.txt content %q", got)
	}
	again, _ := rt.tick()
	if again.Digest != snap.Digest {
		t.Fatalf("recapture digest %s, want %s", again.Digest, snap.Digest)
	}

	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "restored")})
	rt.assertRestored(snap, r, want)
	flags := rt.git(r.Path, "ls-files", "-v")
	for _, line := range []string{"h au.txt", "S sw.txt", "h au-del.txt", "h au-chmod.sh", "H sw-same.txt"} {
		if !slices.Contains(strings.Split(flags, "\n"), line) {
			t.Fatalf("restored index flags %q lack %q", flags, line)
		}
	}
}

func TestCaptureJJTrackedIgnored(t *testing.T) {
	f := vcstest.New(t)
	repo := f.JJClone(filepath.Join(f.Root, "repo"))
	ws := f.JJWorkspace(repo, filepath.Join(f.Root, "ws"), "second")
	f.WriteFile(ws, "build.gen", "generated v1\n")
	f.RunJJ(ws, "status")
	f.WriteFile(ws, ".gitignore", "*.gen\n")
	f.WriteFile(ws, "build.gen", "generated v2\n")
	f.WriteFile(ws, "fresh.gen", "never tracked\n")

	sink := worktreetest.New()
	snap := mustCapture(t, openStore(t), discoverAt(t, repo, ws), sink)
	if got := filePaths(snap); !snap.Complete || !slices.Equal(got, []string{"file:.gitignore", "file:build.gen"}) {
		t.Fatalf("snapshot complete=%v files=%v, want .gitignore and the @-tracked build.gen", snap.Complete, got)
	}
	e := mustFileEntry(t, snap, "build.gen")
	if !e.Untracked {
		t.Fatalf("build.gen %+v, want an untracked entry", e)
	}
	if got := readArtifact(t, sink, *e.Content); got != "generated v2\n" {
		t.Fatalf("build.gen content %q, want the current bytes", got)
	}
}

func mustFileEntry(t *testing.T, snap worktree.Snapshot, path string) worktree.FileEntry {
	t.Helper()
	e, ok := fileEntry(snap, path)
	if !ok || e.Content == nil {
		t.Fatalf("no captured content for %s in %v", path, filePaths(snap))
	}
	return e
}

func fsmonitorHook(t *testing.T, f *vcstest.Fixture, ran string) string {
	t.Helper()
	hook := filepath.Join(f.Root, ran+"-hook")
	//nolint:gosec // G306: the fsmonitor hook must be executable; it lives in a test-controlled temp dir.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+filepath.Join(f.Root, ran)+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return hook
}
