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
