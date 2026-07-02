package vcs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestOpInProgress proves opInProgress reports every git/jj live-operation marker
// by presence alone (never by a timestamp), and reads idle when none is present.
// Markers are created under a real colocated clone so the .git/.jj paths match the
// production layout exactly.
func TestOpInProgress(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))

	if reason, err := opInProgress(dest); err != nil || reason != "" {
		t.Fatalf("idle opInProgress = (%q, %v), want (\"\", nil)", reason, err)
	}

	tests := []struct {
		name   string
		rel    string
		dir    bool
		reason string
	}{
		{"git index lock", filepath.Join(".git", "index.lock"), false, "git index locked"},
		{"git packed-refs lock", filepath.Join(".git", "packed-refs.lock"), false, "git refs locked"},
		{"git merge head", filepath.Join(".git", "MERGE_HEAD"), false, "merge in progress"},
		{"git rebase-merge dir", filepath.Join(".git", "rebase-merge"), true, "rebase in progress"},
		{"git rebase-apply dir", filepath.Join(".git", "rebase-apply"), true, "rebase in progress"},
		{"git cherry-pick head", filepath.Join(".git", "CHERRY_PICK_HEAD"), false, "cherry-pick in progress"},
		{"git revert head", filepath.Join(".git", "REVERT_HEAD"), false, "revert in progress"},
		{"git bisect log", filepath.Join(".git", "BISECT_LOG"), false, "bisect in progress"},
		{"jj working copy lock", filepath.Join(".jj", "working_copy", "working_copy.lock"), false, "jj operation in progress"},
		{"jj git import lock", filepath.Join(".jj", "repo", "git_import_export.lock"), false, "jj importing git refs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dest, tc.rel)
			create(t, p, tc.dir)

			reason, err := opInProgress(dest)
			if err != nil {
				t.Fatalf("opInProgress: %v", err)
			}
			if reason != tc.reason {
				t.Fatalf("opInProgress = %q, want %q", reason, tc.reason)
			}

			if err := os.Remove(p); err != nil {
				t.Fatalf("remove marker: %v", err)
			}
			if reason, err := opInProgress(dest); err != nil || reason != "" {
				t.Fatalf("post-cleanup opInProgress = (%q, %v), want idle", reason, err)
			}
		})
	}
}

// TestOpInProgressFirstHitWins proves the git probe precedes the jj probe and the
// first marker in order wins: with both a git and a jj lock present, the git index
// lock is reported.
func TestOpInProgressFirstHitWins(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))

	create(t, filepath.Join(dest, ".git", "index.lock"), false)
	create(t, filepath.Join(dest, ".jj", "working_copy", "working_copy.lock"), false)

	reason, err := opInProgress(dest)
	if err != nil {
		t.Fatalf("opInProgress: %v", err)
	}
	if reason != "git index locked" {
		t.Fatalf("opInProgress = %q, want git index locked (git probe first)", reason)
	}
}

func create(t *testing.T, path string, dir bool) {
	t.Helper()
	if dir {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatalf("mkdir marker: %v", err)
		}
		return
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}
