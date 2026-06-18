package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/yasyf/reposync/internal/state"
)

func TestWatchSetColocatedJJ(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(abs, ".jj"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := watchSet(abs)
	want := []string{
		filepath.Join(abs, ".jj", "repo", "op_heads", "heads"),
		filepath.Join(abs, ".git", "refs", "remotes", "origin"),
		filepath.Join(abs, ".git"),
	}
	assertPaths(t, got, want)
}

func TestWatchSetPlainGit(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "repo") // no .jj on disk
	got := watchSet(abs)
	want := []string{
		filepath.Join(abs, ".git", "refs", "remotes", "origin"),
		filepath.Join(abs, ".git"),
		filepath.Join(abs, ".git", "logs", "refs", "remotes", "origin"),
	}
	assertPaths(t, got, want)
}

func TestMatchRepoLongestPrefixWins(t *testing.T) {
	root := "/Users/yasyf/Code"
	parent := state.Repo{Relpath: "Forge"}
	nested := state.Repo{Relpath: "Forge/private-ai"}
	watched := []watchedRepo{
		{repo: parent, abs: filepath.Join(root, "Forge")},
		{repo: nested, abs: filepath.Join(root, "Forge", "private-ai")},
	}

	cases := []struct {
		id   string
		path string
		want string
		hit  bool
	}{
		{"nested-ref", filepath.Join(root, "Forge", "private-ai", ".git", "refs", "remotes", "origin", "main"), "Forge/private-ai", true},
		{"parent-ref", filepath.Join(root, "Forge", ".git", "HEAD"), "Forge", true},
		{"repo-root-itself", filepath.Join(root, "Forge"), "Forge", true},
		{"sibling-no-match", filepath.Join(root, "Forgery", ".git"), "", false},
		{"unrelated", filepath.Join(root, "other", ".git"), "", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			repo, ok := matchRepo(c.path, watched)
			if ok != c.hit {
				t.Fatalf("matchRepo(%q) hit = %v, want %v", c.path, ok, c.hit)
			}
			if ok && repo.Relpath != c.want {
				t.Errorf("matchRepo(%q) = %q, want %q", c.path, repo.Relpath, c.want)
			}
		})
	}
}

func TestIsUnderTmp(t *testing.T) {
	location := "/Users/yasyf/Code"
	tmpPrefix := filepath.Join(location, tmpDirName)
	cases := []struct {
		id   string
		path string
		want bool
	}{
		{"clone-in-flight", filepath.Join(tmpPrefix, "abc", ".git"), true},
		{"tmp-root", tmpPrefix, true},
		{"real-repo", filepath.Join(location, "cc-review", ".git"), false},
		{"prefix-collision", filepath.Join(location, ".reposync-tmpx", ".git"), false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := isUnderTmp(c.path, tmpPrefix); got != c.want {
				t.Errorf("isUnderTmp(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestRealFsnotifyObservesWrite is the one integration test against real
// fsnotify: write a file into a watched directory and assert an event arrives.
// It is bounded by a short context so it can never hang.
func TestRealFsnotifyObservesWrite(t *testing.T) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	dir := t.TempDir()
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := os.WriteFile(filepath.Join(dir, "origin-main"), []byte("deadbeef"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("no fsnotify event observed within timeout")
	case err := <-w.Errors:
		t.Fatalf("fsnotify error: %v", err)
	case ev := <-w.Events:
		if !underRoot(ev.Name, dir) {
			t.Errorf("event path %q not under watched dir %q", ev.Name, dir)
		}
	}
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
