package vcs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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
		{"git sequencer dir", filepath.Join(".git", "sequencer"), true, "sequencer in progress"},
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

// TestOpInProgressExportedWrapper pins the exported wrapper over the probe: it
// reports the marker reason while a lock is held and "" once the repo is idle.
func TestOpInProgressExportedWrapper(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))

	if reason, err := OpInProgress(dest); err != nil || reason != "" {
		t.Fatalf("idle OpInProgress = (%q, %v), want (\"\", nil)", reason, err)
	}

	create(t, filepath.Join(dest, ".git", "index.lock"), false)
	reason, err := OpInProgress(dest)
	if err != nil {
		t.Fatalf("OpInProgress: %v", err)
	}
	if reason != "git index locked" {
		t.Fatalf("OpInProgress = %q, want git index locked", reason)
	}
}

// TestClearStaleLocks proves the janitor removes each janitor-clearable lock-file
// marker once its mtime is older than staleLockAge: the file is gone and the repo
// reads idle afterward. The jj locks are unheld here, so the flock probe finds a dead
// holder and reclaims them. A real colocated clone gives the .git/.jj layout the
// production code walks.
func TestClearStaleLocks(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	stale := time.Now().Add(-2 * time.Hour)

	tests := []struct {
		name   string
		rel    string
		reason string
	}{
		{"git packed-refs lock", filepath.Join(".git", "packed-refs.lock"), "git refs locked"},
		{"jj working copy lock", filepath.Join(".jj", "working_copy", "working_copy.lock"), "jj operation in progress"},
		{"jj git import lock", filepath.Join(".jj", "repo", "git_import_export.lock"), "jj importing git refs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dest, tc.rel)
			create(t, p, false)
			if err := os.Chtimes(p, stale, stale); err != nil {
				t.Fatalf("backdate lock: %v", err)
			}
			if reason, err := opInProgress(dest); err != nil || reason != tc.reason {
				t.Fatalf("pre-clear opInProgress = (%q, %v), want (%q, nil)", reason, err, tc.reason)
			}

			cleared, err := ClearStaleLocks(dest)
			if err != nil {
				t.Fatalf("ClearStaleLocks: %v", err)
			}
			if len(cleared) != 1 || cleared[0] != tc.rel {
				t.Fatalf("cleared = %v, want [%q]", cleared, tc.rel)
			}
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("stat removed lock = %v, want not-exist", err)
			}
			if reason, err := opInProgress(dest); err != nil || reason != "" {
				t.Fatalf("post-clear opInProgress = (%q, %v), want idle", reason, err)
			}
		})
	}
}

// TestClearStaleLocksKeepsIndexLock proves index.lock is never janitor-cleared, even
// backdated well past staleLockAge: a commit-editor session or a slow smudge-filter
// checkout can legitimately hold it that long with an untouched mtime, so unlinking it
// would defeat git's mutual exclusion. The lock stays and the repo still reads busy.
func TestClearStaleLocksKeepsIndexLock(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	p := filepath.Join(dest, ".git", "index.lock")
	create(t, p, false)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(p, stale, stale); err != nil {
		t.Fatalf("backdate index.lock: %v", err)
	}

	cleared, err := ClearStaleLocks(dest)
	if err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none (index.lock never cleared)", cleared)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("index.lock removed: %v", err)
	}
	if reason, err := opInProgress(dest); err != nil || reason != "git index locked" {
		t.Fatalf("opInProgress = (%q, %v), want (git index locked, nil)", reason, err)
	}
}

// TestClearStaleLocksSkipsLiveJJLock proves a backdated jj lock with a live flock
// holder survives the janitor: jj guards its locks with an flock the kernel only drops
// when the holder dies, so a live process's lock must never be unlinked. The test holds
// the flock from a separate open file description (flock ownership is per-open-file-
// description, so this conflicts even in-process), then releases it and confirms the now
// dead-held lock is reclaimed.
func TestClearStaleLocksSkipsLiveJJLock(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	rel := filepath.Join(".jj", "working_copy", "working_copy.lock")
	p := filepath.Join(dest, rel)
	create(t, p, false)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(p, stale, stale); err != nil {
		t.Fatalf("backdate jj lock: %v", err)
	}

	//nolint:gosec // G304: p is a lock path inside this test's t.TempDir repo.
	held, err := os.OpenFile(p, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open jj lock: %v", err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold flock: %v", err)
	}

	cleared, err := ClearStaleLocks(dest)
	if err != nil {
		t.Fatalf("ClearStaleLocks (live holder): %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none (live flock holder)", cleared)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("live-held jj lock removed: %v", err)
	}
	if reason, err := opInProgress(dest); err != nil || reason != "jj operation in progress" {
		t.Fatalf("opInProgress = (%q, %v), want (jj operation in progress, nil)", reason, err)
	}

	// Holder dies -> the kernel drops the flock -> the janitor reclaims the lock.
	if err := held.Close(); err != nil {
		t.Fatalf("close held: %v", err)
	}

	cleared, err = ClearStaleLocks(dest)
	if err != nil {
		t.Fatalf("ClearStaleLocks (dead holder): %v", err)
	}
	if len(cleared) != 1 || cleared[0] != rel {
		t.Fatalf("cleared = %v, want [%q]", cleared, rel)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("stat reclaimed jj lock = %v, want not-exist", err)
	}
}

// TestClearStaleLocksKeepsFreshLock proves a lock younger than staleLockAge is a
// live holder: the janitor leaves it in place and the repo still reads busy.
func TestClearStaleLocksKeepsFreshLock(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	p := filepath.Join(dest, ".git", "packed-refs.lock")
	create(t, p, false)

	cleared, err := ClearStaleLocks(dest)
	if err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none (fresh lock)", cleared)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fresh lock removed: %v", err)
	}
	if reason, err := opInProgress(dest); err != nil || reason != "git refs locked" {
		t.Fatalf("opInProgress = (%q, %v), want (git refs locked, nil)", reason, err)
	}
}

// TestClearStaleLocksNeverTouchesStateMarkers proves a merge/rebase marker — user
// intent, not a lock — is never removed even when backdated well past staleLockAge.
func TestClearStaleLocksNeverTouchesStateMarkers(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	stale := time.Now().Add(-2 * time.Hour)

	merge := filepath.Join(dest, ".git", "MERGE_HEAD")
	rebase := filepath.Join(dest, ".git", "rebase-merge")
	create(t, merge, false)
	create(t, rebase, true)
	for _, p := range []string{merge, rebase} {
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatalf("backdate %s: %v", p, err)
		}
	}

	cleared, err := ClearStaleLocks(dest)
	if err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none (state markers never removed)", cleared)
	}
	if _, err := os.Stat(merge); err != nil {
		t.Fatalf("MERGE_HEAD removed: %v", err)
	}
	if _, err := os.Stat(rebase); err != nil {
		t.Fatalf("rebase-merge removed: %v", err)
	}
	if reason, err := opInProgress(dest); err != nil || reason != "merge in progress" {
		t.Fatalf("opInProgress = (%q, %v), want (merge in progress, nil)", reason, err)
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

// TestOpInProgressLinkedWorktree proves a linked worktree (whose .git is a
// pointer file) and a secondary jj workspace (whose .jj/repo is a pointer file)
// probe without ENOTDIR: per-worktree markers come from the worktree's own
// admin dir, shared markers from the common dir and the jj repo dir.
func TestOpInProgressLinkedWorktree(t *testing.T) {
	f := vcstest.New(t)
	main := f.JJClone(filepath.Join(f.Root, "main"))
	linked := f.LinkedWorktree(main, filepath.Join(f.Root, "linked"), "feature")
	ws := f.JJWorkspace(main, filepath.Join(f.Root, "ws"), "second")
	linkedAdmin := filepath.Join(main, ".git", "worktrees", "linked")

	tests := []struct {
		name   string
		marker string
		dir    bool
		probe  string
		reason string
	}{
		{"linked idle", "", false, linked, ""},
		{"main index lock is not the linked worktree's", filepath.Join(main, ".git", "index.lock"), false, linked, ""},
		{"linked index lock", filepath.Join(linkedAdmin, "index.lock"), false, linked, "git index locked"},
		{"linked sequencer", filepath.Join(linkedAdmin, "sequencer"), true, linked, "sequencer in progress"},
		{"common packed-refs lock", filepath.Join(main, ".git", "packed-refs.lock"), false, linked, "git refs locked"},
		{"jj workspace idle", "", false, ws, ""},
		{"jj workspace working copy lock", filepath.Join(ws, ".jj", "working_copy", "working_copy.lock"), false, ws, "jj operation in progress"},
		{"jj repo import lock seen from workspace", filepath.Join(main, ".jj", "repo", "git_import_export.lock"), false, ws, "jj importing git refs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.marker != "" {
				create(t, tc.marker, tc.dir)
				t.Cleanup(func() {
					if err := os.RemoveAll(tc.marker); err != nil {
						t.Fatalf("remove marker: %v", err)
					}
				})
			}
			reason, err := OpInProgress(tc.probe)
			if err != nil {
				t.Fatalf("OpInProgress: %v", err)
			}
			if reason != tc.reason {
				t.Fatalf("OpInProgress = %q, want %q", reason, tc.reason)
			}
		})
	}
}

// TestOpStateLockFlag proves OpState separates lock files a live command holds
// (lock=true: retry) from multi-step operation state (lock=false: defer).
func TestOpStateLockFlag(t *testing.T) {
	f := vcstest.New(t)
	main := f.JJClone(filepath.Join(f.Root, "main"))
	gitDir := filepath.Join(main, ".git")
	jjDir := filepath.Join(main, ".jj")

	tests := []struct {
		name   string
		marker string
		dir    bool
		reason string
		lock   bool
	}{
		{"index lock", filepath.Join(gitDir, "index.lock"), false, "git index locked", true},
		{"packed-refs lock", filepath.Join(gitDir, "packed-refs.lock"), false, "git refs locked", true},
		{"merge head", filepath.Join(gitDir, "MERGE_HEAD"), false, "merge in progress", false},
		{"sequencer", filepath.Join(gitDir, "sequencer"), true, "sequencer in progress", false},
		{"jj working copy lock", filepath.Join(jjDir, "working_copy", "working_copy.lock"), false, "jj operation in progress", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			create(t, tc.marker, tc.dir)
			defer func() {
				if err := os.RemoveAll(tc.marker); err != nil {
					t.Fatalf("remove marker: %v", err)
				}
			}()
			reason, lock, err := OpState(gitDir, gitDir, jjDir)
			if err != nil {
				t.Fatalf("OpState: %v", err)
			}
			if reason != tc.reason || lock != tc.lock {
				t.Fatalf("OpState = (%q, %v), want (%q, %v)", reason, lock, tc.reason, tc.lock)
			}
		})
	}
	if reason, lock, err := OpState("", "", ""); err != nil || reason != "" || lock {
		t.Fatalf("OpState with no dirs = (%q, %v, %v), want idle", reason, lock, err)
	}
}
