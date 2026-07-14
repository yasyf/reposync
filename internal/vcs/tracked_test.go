package vcs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestTrackedNames pins the git index gate: a committed .env reads as tracked while
// an untracked sibling and an absent name do not.
func TestTrackedNames(t *testing.T) {
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "clone"))
	f.WriteFile(repo, ".env", "A=1\n")
	f.WriteFile(repo, ".env.local", "B=2\n")
	f.RunGit(repo, "add", ".env")
	f.RunGit(repo, "commit", "-qm", "add tracked env")

	tracked, err := TrackedNames(context.Background(), repo, []string{".env", ".env.local", ".env.absent"})
	if err != nil {
		t.Fatalf("TrackedNames: %v", err)
	}
	want := map[string]bool{".env": true}
	if len(tracked) != len(want) {
		t.Fatalf("tracked = %v, want %v", tracked, want)
	}
	for name, ok := range want {
		if tracked[name] != ok {
			t.Errorf("tracked[%q] = %v, want %v", name, tracked[name], ok)
		}
	}
	if tracked[".env.local"] {
		t.Error("tracked[.env.local] = true, want untracked file excluded")
	}
}

// TestTrackedNamesEmpty pins the no-op fast path: an empty names slice returns an
// empty set without shelling out to git.
func TestTrackedNamesEmpty(t *testing.T) {
	tracked, err := TrackedNames(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("TrackedNames: %v", err)
	}
	if len(tracked) != 0 {
		t.Errorf("tracked = %v, want empty", tracked)
	}
}
