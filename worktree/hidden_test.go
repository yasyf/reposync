package worktree_test

import (
	"maps"
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
	for _, flag := range []string{"", "--assume-unchanged", "--skip-worktree"} {
		t.Run("flag="+flag, func(t *testing.T) {
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
			if flag != "" {
				f.RunGit(repo, "update-index", flag, "sub")
			}
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
			if flag != "" {
				f.RunGit(inner, "status")
			}
			for _, ran := range sentinels {
				if !f.FileExists(f.Root, ran) {
					t.Fatalf("a plain git status never left %s; the fixture proves nothing", ran)
				}
			}
		})
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

func TestRoundTripHiddenIntentToAdd(t *testing.T) {
	f := vcstest.New(t)
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
	f.WriteFile(rt.src, "au.txt", "intent hidden by assume-unchanged\n")
	f.WriteFile(rt.src, "sw-empty.txt", "")
	f.RunGit(rt.src, "add", "-N", "au.txt", "sw-empty.txt")
	f.RunGit(rt.src, "update-index", "--assume-unchanged", "au.txt")
	f.RunGit(rt.src, "update-index", "--skip-worktree", "sw-empty.txt")
	if st := rt.status(rt.src); len(st) != 0 {
		t.Fatalf("git status shows %q; the flags hide nothing", st)
	}

	snap, want := rt.tick()
	if !snap.Complete || !slices.Equal(snap.IntentToAdd, []string{"au.txt", "sw-empty.txt"}) {
		t.Fatalf("complete=%v intent-to-add %v, want both hidden intent-to-add paths", snap.Complete, snap.IntentToAdd)
	}
	for path, e := range map[string]worktree.FileEntry{
		"au.txt":       {Kind: worktree.FileRegular, AssumeUnchanged: true},
		"sw-empty.txt": {Kind: worktree.FileRegular, SkipWorktree: true},
	} {
		got := mustFileEntry(t, snap, path)
		if got.Kind != e.Kind || got.AssumeUnchanged != e.AssumeUnchanged || got.SkipWorktree != e.SkipWorktree || got.Untracked {
			t.Fatalf("%s captured as %+v, want %+v", path, got, e)
		}
	}

	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "restored")})
	rt.assertRestored(snap, r, want)
	flags := strings.Split(rt.git(r.Path, "ls-files", "-v"), "\n")
	for _, line := range []string{"h au.txt", "S sw-empty.txt"} {
		if !slices.Contains(flags, line) {
			t.Fatalf("restored index flags %q lack %q", flags, line)
		}
	}
	f.RunGit(r.Path, "update-index", "--no-assume-unchanged", "au.txt")
	f.RunGit(r.Path, "update-index", "--no-skip-worktree", "sw-empty.txt")
	st := rt.status(r.Path)
	if len(st) != 2 || !strings.HasPrefix(st[0], "1 .A ") || !strings.HasSuffix(st[0], " au.txt") || !strings.HasPrefix(st[1], "1 .A ") || !strings.HasSuffix(st[1], " sw-empty.txt") {
		t.Fatalf("unflagged restored status %q, want both paths intent-to-add", st)
	}
}

func TestCaptureSparseDeletion(t *testing.T) {
	tests := []struct {
		name string
		set  []string
	}{
		{"non-cone", []string{"--no-cone", "/*", "!/excluded/"}},
		{"cone", []string{"--cone", "kept"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			for _, dir := range []string{"kept", "excluded"} {
				if err := os.MkdirAll(filepath.Join(f.Seed, dir), 0o750); err != nil {
					t.Fatal(err)
				}
				f.WriteFile(filepath.Join(f.Seed, dir), "f.txt", dir+"\n")
			}
			f.RunGit(f.Seed, "add", "-A")
			f.RunGit(f.Seed, "commit", "-qm", "sparse base")
			f.RunGit(f.Seed, "push", "-q", "origin", "main")
			repo := f.GitClone(filepath.Join(f.Root, "repo"))
			f.RunGit(repo, append([]string{"sparse-checkout", "set"}, tt.set...)...)
			if f.FileExists(repo, "excluded/f.txt") || !f.FileExists(repo, "kept/f.txt") {
				t.Fatal("sparse-checkout left excluded/ or dropped kept/; the fixture proves nothing")
			}
			f.RunGit(repo, "update-index", "--skip-worktree", "README.md", "kept/f.txt")
			for _, p := range []string{"README.md", "kept/f.txt"} {
				if err := os.Remove(filepath.Join(repo, p)); err != nil {
					t.Fatal(err)
				}
			}

			before := trees(f, filepath.Join(repo, ".git"))
			snap := mustCapture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New())
			if after := trees(f, filepath.Join(repo, ".git")); !maps.Equal(before, after) {
				t.Fatalf("capture wrote the source repository: %q", changed(before, after))
			}
			if got := filePaths(snap); !snap.Complete || len(got) != 0 {
				t.Fatalf("snapshot complete=%v files=%v, want the hidden paths carried by the sparse state alone", snap.Complete, got)
			}
			if snap.Sparse == nil || !slices.Equal(snap.Sparse.Exceptions, []string{"README.md", "kept/f.txt"}) {
				t.Fatalf("sparse %+v, want the two hidden-inside exceptions and not excluded/f.txt", snap.Sparse)
			}
			if snap.Sparse == nil || len(snap.Sparse.Patterns) == 0 {
				t.Fatalf("sparse %+v, want the recorded patterns", snap.Sparse)
			}
		})
	}
}

func TestCaptureJJWorkspaceSubmoduleReadOnly(t *testing.T) {
	f := vcstest.New(t)
	sub := filepath.Join(f.Root, "subrepo")
	f.RunGit(f.Root, "init", "-q", "-b", "main", sub)
	f.ConfigGit(sub)
	f.WriteFile(sub, ".gitattributes", "*.dat filter=subspy\n")
	f.WriteFile(sub, "x.dat", "data\n")
	f.RunGit(sub, "add", "-A")
	f.RunGit(sub, "commit", "-q", "-m", "sub")
	f.RunGit(f.Seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	f.RunGit(f.Seed, "commit", "-q", "-m", "add sub")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	repo := f.JJClone(filepath.Join(f.Root, "repo"))
	ws := f.JJWorkspace(repo, filepath.Join(f.Root, "ws"), "second")
	inner := filepath.Join(ws, "sub")
	f.RunGit(f.Root, "clone", "-q", sub, inner)
	sentinel := filepath.Join(f.Root, "sub-filter-ran")
	f.RunGit(inner, "config", "filter.subspy.clean", "touch '"+sentinel+"'; cat")
	f.RunGit(inner, "config", "filter.subspy.required", "true")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(inner, "x.dat"), future, future); err != nil {
		t.Fatal(err)
	}
	f.WriteFile(ws, "wip.txt", "wip\n")

	snap := mustCapture(t, openStore(t), discoverAt(t, repo, ws), worktreetest.New())
	if f.FileExists(f.Root, "sub-filter-ran") {
		t.Fatal("Capture ran a submodule filter in a jj workspace")
	}
	if got := filePaths(snap); !snap.Complete || !slices.Equal(got, []string{"file:wip.txt"}) {
		t.Fatalf("snapshot complete=%v files=%v, want complete with wip.txt", snap.Complete, got)
	}
	f.RunGit(inner, "status")
	if !f.FileExists(f.Root, "sub-filter-ran") {
		t.Fatal("a plain git status never ran the submodule filter; the fixture proves nothing")
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

func TestRoundTripDeletedIntentToAdd(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		flag  string
		entry worktree.FileEntry
		index string
	}{
		{"unflagged", "ita.txt", "", worktree.FileEntry{Kind: worktree.FileDeleted}, "H ita.txt"},
		{"nested", "dir/sub/ita.txt", "", worktree.FileEntry{Kind: worktree.FileDeleted}, "H dir/sub/ita.txt"},
		{"assume-unchanged", "au.txt", "--assume-unchanged", worktree.FileEntry{Kind: worktree.FileDeleted, AssumeUnchanged: true}, "h au.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
			dir, name := filepath.Split(filepath.FromSlash(tt.path))
			if err := os.MkdirAll(filepath.Join(rt.src, dir), 0o750); err != nil {
				t.Fatal(err)
			}
			f.WriteFile(filepath.Join(rt.src, dir), name, "about to vanish\n")
			f.RunGit(rt.src, "add", "-N", tt.path)
			if tt.flag != "" {
				f.RunGit(rt.src, "update-index", tt.flag, tt.path)
			}
			if err := os.RemoveAll(filepath.Join(rt.src, strings.Split(tt.path, "/")[0])); err != nil {
				t.Fatal(err)
			}

			snap, want := rt.tick()
			if !snap.Complete || !slices.Equal(snap.IntentToAdd, []string{tt.path}) {
				t.Fatalf("complete=%v intent-to-add %v, want [%s]", snap.Complete, snap.IntentToAdd, tt.path)
			}
			i := slices.IndexFunc(snap.Files, func(e worktree.FileEntry) bool { return e.Path == tt.path })
			if i < 0 {
				t.Fatalf("no file entry for %s in %v", tt.path, filePaths(snap))
			}
			if got := snap.Files[i]; got.Kind != tt.entry.Kind || got.AssumeUnchanged != tt.entry.AssumeUnchanged || got.SkipWorktree != tt.entry.SkipWorktree {
				t.Fatalf("%s captured as %+v, want %+v", tt.path, snap.Files[i], tt.entry)
			}

			r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "restored")})
			rt.assertRestored(snap, r, want)
			if f.FileExists(r.Path, tt.path) {
				t.Fatalf("restored worktree still holds the intent-to-add placeholder %s", tt.path)
			}
			if flags := strings.Split(rt.git(r.Path, "ls-files", "-v"), "\n"); !slices.Contains(flags, tt.index) {
				t.Fatalf("restored index %q lacks %q", flags, tt.index)
			}
			if src, got := rt.git(rt.src, "diff", "--name-status", "--ita-visible-in-index"), rt.git(r.Path, "diff", "--name-status", "--ita-visible-in-index"); src != got {
				t.Fatalf("intent-to-add view differs:\nsource   %q\nrestored %q", src, got)
			}
		})
	}
}
