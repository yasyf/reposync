package vcs

import (
	"fmt"
	"os"
	"path/filepath"
)

// gitInProgressMarkers are the lock and state files git drops under .git while a
// command holds the index or a multi-step operation is mid-flight. Presence is the
// signal; nothing is parsed. A colocated jj repo has a real .git directory, so
// these apply to both backends. packed-refs.lock is the exact symptom that
// orphaned the reported commits — a live ref transaction (fetch/commit/gc).
var gitInProgressMarkers = []struct {
	name   string
	reason string
}{
	{"index.lock", "git index locked"},
	{"packed-refs.lock", "git refs locked"},
	{"MERGE_HEAD", "merge in progress"},
	{"rebase-merge", "rebase in progress"},
	{"rebase-apply", "rebase in progress"},
	{"CHERRY_PICK_HEAD", "cherry-pick in progress"},
	{"REVERT_HEAD", "revert in progress"},
	{"BISECT_LOG", "bisect in progress"},
}

// jjInProgressMarkers are the lock files jj holds around a live working-copy or
// git-import operation, relative to .jj. jj creates and removes both around each
// operation, so presence means an operation is live right now (jj blocks other
// commands on working_copy.lock rather than racing them).
var jjInProgressMarkers = []struct {
	rel    string
	reason string
}{
	{filepath.Join("working_copy", "working_copy.lock"), "jj operation in progress"},
	{filepath.Join("repo", "git_import_export.lock"), "jj importing git refs"},
}

// OpInProgress reports a live git or jj operation under root by lock-marker
// presence alone, or "" when the repo is idle. It never shells out, so it is safe
// to probe a locked repo.
func OpInProgress(root string) (string, error) {
	return opInProgress(root)
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
