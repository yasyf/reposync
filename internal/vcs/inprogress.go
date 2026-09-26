package vcs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// staleLockAge is how old a lock file must be before its holder is presumed
// dead: 6× the opTimeout bound on any reposync-driven holder, and past the
// default idle threshold — the system's existing "certainly idle" horizon —
// so a live user-held lock (even a commit-editor session) is implausible.
const staleLockAge = 30 * time.Minute

// marker is a file git or jj drops while an operation is mid-flight. Presence
// is the signal; nothing is parsed. lock marks a lock file a live command holds
// (transient: retry later) as opposed to a state marker a multi-step operation
// leaves until the user finishes it. clearable marks a lock the janitor may
// remove once stale: packed-refs.lock is the exact symptom that orphaned the
// reported commits, and no legitimate holder keeps it for 30m. index.lock is
// not clearable: a commit-editor session or a slow smudge-filter checkout can
// hold it past staleLockAge with an untouched mtime, and unlinking a live lock
// defeats mutual exclusion. State markers are never removable.
type marker struct {
	rel       string
	reason    string
	lock      bool
	clearable bool
}

var gitWorktreeMarkers = []marker{
	{"index.lock", "git index locked", true, false},
	{"MERGE_HEAD", "merge in progress", false, false},
	{"rebase-merge", "rebase in progress", false, false},
	{"rebase-apply", "rebase in progress", false, false},
	{"CHERRY_PICK_HEAD", "cherry-pick in progress", false, false},
	{"REVERT_HEAD", "revert in progress", false, false},
	{"BISECT_LOG", "bisect in progress", false, false},
	{"sequencer", "sequencer in progress", false, false},
}

var gitCommonMarkers = []marker{
	{"packed-refs.lock", "git refs locked", true, true},
}

// jj creates and removes these around each operation, so presence means an
// operation is live right now (jj blocks other commands on working_copy.lock
// rather than racing them).
var (
	jjWorkspaceMarkers = []marker{
		{filepath.Join("working_copy", "working_copy.lock"), "jj operation in progress", true, true},
	}
	jjRepoMarkers = []marker{
		{"git_import_export.lock", "jj importing git refs", true, true},
	}
)

// OpInProgress reports a live git or jj operation under root by lock-marker
// presence alone, or "" when the repo is idle. It never shells out, so it is safe
// to probe a locked repo. A linked worktree's markers are read from its admin
// dir and the shared common dir.
func OpInProgress(root string) (string, error) {
	return opInProgress(root)
}

// OpState reports the first live git or jj operation marker under a worktree's
// per-worktree admin dir (gitDir), the repository's common dir, and the
// workspace's .jj dir, by presence alone and without a subprocess. An empty dir
// argument is not probed. lock is true when the marker is a lock file a live
// command holds (retry later) and false for a multi-step operation state such
// as a merge, rebase, or sequencer run.
func OpState(gitDir, commonDir, jjDir string) (reason string, lock bool, err error) {
	dirs, err := markerDirs(gitDir, commonDir, jjDir)
	if err != nil {
		return "", false, err
	}
	for _, d := range dirs {
		for _, m := range d.markers {
			present, err := exists(filepath.Join(d.dir, m.rel))
			if err != nil {
				return "", false, err
			}
			if present {
				return m.reason, m.lock, nil
			}
		}
	}
	return "", false, nil
}

// GitDirs resolves the per-worktree admin dir and the common dir of the git
// worktree at root from files alone: a .git directory is both, and a .git
// pointer file names the admin dir, whose commondir file (when present) names
// the common dir. Both are "" when root has no .git.
func GitDirs(root string) (gitDir, commonDir string, err error) {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if os.IsNotExist(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("stat %s: %w", dotGit, err)
	}
	if info.IsDir() {
		return dotGit, dotGit, nil
	}
	gitDir, err = readPointer(dotGit, "gitdir: ", root)
	if err != nil {
		return "", "", err
	}
	commonDir, err = readPointer(filepath.Join(gitDir, "commondir"), "", gitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return gitDir, gitDir, nil
	}
	if err != nil {
		return "", "", err
	}
	return gitDir, commonDir, nil
}

// ClearStaleLocks removes the lock-file markers under root whose mtime is older
// than staleLockAge — a provably dead holder — and returns the repo-relative
// paths removed. State markers (merge, rebase, …) are never touched.
func ClearStaleLocks(root string) ([]string, error) {
	gitDir, commonDir, err := GitDirs(root)
	if err != nil {
		return nil, err
	}
	dirs, err := markerDirs(gitDir, commonDir, filepath.Join(root, ".jj"))
	if err != nil {
		return nil, err
	}
	var cleared []string
	for _, d := range dirs {
		for _, m := range d.markers {
			if !m.clearable {
				continue
			}
			path := filepath.Join(d.dir, m.rel)
			removed, err := removeIfStale(path, d.probeFlock)
			if err != nil {
				return nil, err
			}
			if !removed {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, fmt.Errorf("relativize %s: %w", path, err)
			}
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
	//nolint:gosec // G304: path is a lock file inside a repo reposync manages, from internal call sites, not untrusted input.
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open jj lock %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
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

func opInProgress(root string) (string, error) {
	gitDir, commonDir, err := GitDirs(root)
	if err != nil {
		return "", err
	}
	reason, _, err := OpState(gitDir, commonDir, filepath.Join(root, ".jj"))
	return reason, err
}

type markerDir struct {
	dir     string
	markers []marker
	// git guards its locks with O_EXCL presence locking, not flock, so a probe
	// proves nothing about the holder — age alone gates a git clear.
	probeFlock bool
}

func markerDirs(gitDir, commonDir, jjDir string) ([]markerDir, error) {
	var dirs []markerDir
	if gitDir != "" {
		dirs = append(dirs, markerDir{dir: gitDir, markers: gitWorktreeMarkers})
	}
	if commonDir != "" {
		dirs = append(dirs, markerDir{dir: commonDir, markers: gitCommonMarkers})
	}
	if jjDir == "" {
		return dirs, nil
	}
	repoDir, err := jjRepoDir(jjDir)
	if err != nil {
		return nil, err
	}
	return append(dirs,
		markerDir{dir: jjDir, markers: jjWorkspaceMarkers, probeFlock: true},
		markerDir{dir: repoDir, markers: jjRepoMarkers, probeFlock: true},
	), nil
}

func jjRepoDir(jjDir string) (string, error) {
	repo := filepath.Join(jjDir, "repo")
	info, err := os.Stat(repo)
	if os.IsNotExist(err) || (err == nil && info.IsDir()) {
		return repo, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", repo, err)
	}
	return readPointer(repo, "", jjDir)
}

func readPointer(path, prefix, base string) (string, error) {
	//nolint:gosec // G304: path is a git/jj pointer file inside a repo reposync manages.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	target, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), prefix)
	if !ok || target == "" {
		return "", fmt.Errorf("malformed pointer file %s", path)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target), nil
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
