package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// staleLockAge is how old a lock file must be before its holder is presumed
// dead: 6× the opTimeout bound on any reposync-driven holder, and past the
// default idle threshold — the system's existing "certainly idle" horizon —
// so a live user-held lock (even a commit-editor session) is implausible.
const staleLockAge = 30 * time.Minute

// gitInProgressMarkers are the lock and state files git drops under .git while a
// command holds the index or a multi-step operation is mid-flight. Presence is the
// signal; nothing is parsed. A colocated jj repo has a real .git directory, so
// these apply to both backends. packed-refs.lock is the exact symptom that
// orphaned the reported commits — a live ref transaction (fetch/commit/gc) — and
// the only git lock the janitor clears: no legitimate holder keeps it for 30m. lock
// marks a janitor-clearable lock file. index.lock is not one (lock:false): a
// commit-editor session or a slow smudge-filter checkout can hold it past
// staleLockAge with an untouched mtime, and unlinking a live lock defeats mutual
// exclusion. State markers are never removable.
var gitInProgressMarkers = []struct {
	name   string
	reason string
	lock   bool
}{
	{"index.lock", "git index locked", false},
	{"packed-refs.lock", "git refs locked", true},
	{"MERGE_HEAD", "merge in progress", false},
	{"rebase-merge", "rebase in progress", false},
	{"rebase-apply", "rebase in progress", false},
	{"CHERRY_PICK_HEAD", "cherry-pick in progress", false},
	{"REVERT_HEAD", "revert in progress", false},
	{"BISECT_LOG", "bisect in progress", false},
}

// jjInProgressMarkers are the lock files jj holds around a live working-copy or
// git-import operation, relative to .jj. jj creates and removes both around each
// operation, so presence means an operation is live right now (jj blocks other
// commands on working_copy.lock rather than racing them). Both are janitor-clearable.
var jjInProgressMarkers = []struct {
	rel    string
	reason string
	lock   bool
}{
	{filepath.Join("working_copy", "working_copy.lock"), "jj operation in progress", true},
	{filepath.Join("repo", "git_import_export.lock"), "jj importing git refs", true},
}

// OpInProgress reports a live git or jj operation under root by lock-marker
// presence alone, or "" when the repo is idle. It never shells out, so it is safe
// to probe a locked repo.
func OpInProgress(root string) (string, error) {
	return opInProgress(root)
}

// ClearStaleLocks removes the lock-file markers under root whose mtime is older
// than staleLockAge — a provably dead holder — and returns the repo-relative
// paths removed. State markers (merge, rebase, …) are never touched.
func ClearStaleLocks(root string) ([]string, error) {
	var cleared []string
	gitDir := filepath.Join(root, ".git")
	for _, m := range gitInProgressMarkers {
		if !m.lock {
			continue
		}
		rel := filepath.Join(".git", m.name)
		// git guards packed-refs.lock with O_EXCL presence locking, not flock, so a
		// probe proves nothing about the holder — age alone gates the git clear.
		removed, err := removeIfStale(filepath.Join(gitDir, m.name), false)
		if err != nil {
			return nil, err
		}
		if removed {
			cleared = append(cleared, rel)
		}
	}
	jjDir := filepath.Join(root, ".jj")
	for _, m := range jjInProgressMarkers {
		if !m.lock {
			continue
		}
		rel := filepath.Join(".jj", m.rel)
		removed, err := removeIfStale(filepath.Join(jjDir, m.rel), true)
		if err != nil {
			return nil, err
		}
		if removed {
			cleared = append(cleared, rel)
		}
	}
	return cleared, nil
}

func removeIfStale(path string, probeFlock bool) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if time.Since(info.ModTime()) < staleLockAge {
		return false, nil
	}
	if probeFlock {
		return removeIfHolderDead(path)
	}
	return remove(path)
}

// removeIfHolderDead removes a backdated jj lock only after proving its holder is
// gone. jj guards each lock with an flock the kernel drops when the holder dies, so
// acquiring LOCK_EX|LOCK_NB means no live process still holds it; EWOULDBLOCK means a
// holder is alive, so the lock is left in place (not an error). The remove happens
// while the probe lock is held so a concurrent acquirer cannot slip in between.
func removeIfHolderDead(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open jj lock %s: %w", path, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("probe jj lock %s: %w", path, err)
	}
	return remove(path)
}

// remove unlinks a lock file the janitor has cleared to reclaim, tolerating the
// holder deleting it first: an IsNotExist means it was cleaned by the holder between
// the stat and the remove, which is a no-op success, not an error.
func remove(path string) (bool, error) {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove stale lock %s: %w", path, err)
	}
	return true, nil
}

// opInProgress reports a live git or jj operation under repoPath, or "" when the
// repo is idle. It only stats known lock and state markers — presence is the
// signal — so it never shells into (and so never blocks on) a locked repo. The git
// markers are checked for both backends because a colocated jj repo carries a real
// .git directory. A stat failure other than "not exist" is returned, never
// swallowed into a false "idle".
func opInProgress(repoPath string) (string, error) {
	gitDir := filepath.Join(repoPath, ".git")
	for _, m := range gitInProgressMarkers {
		present, err := exists(filepath.Join(gitDir, m.name))
		if err != nil {
			return "", err
		}
		if present {
			return m.reason, nil
		}
	}
	jjDir := filepath.Join(repoPath, ".jj")
	for _, m := range jjInProgressMarkers {
		present, err := exists(filepath.Join(jjDir, m.rel))
		if err != nil {
			return "", err
		}
		if present {
			return m.reason, nil
		}
	}
	return "", nil
}

// exists reports whether path exists, returning an error only for a stat failure
// that is not os.ErrNotExist — a real I/O error must never read as a silent "idle".
func exists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	return true, nil
}
