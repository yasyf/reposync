// Package vcs is the verified jj+git command layer that keeps a tracked repo on
// the latest trunk without ever clobbering in-progress work. It also pushes local
// trunk back to origin, but only as a clean fast-forward when the repo is quiet.
// A repo is colocated jj when a .jj dir exists at its root, otherwise plain git
// when .git exists.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultTrunk = "main"

// ErrNoOrigin is returned by Origin when the repo has no origin remote.
var ErrNoOrigin = errors.New("no origin remote")

// ErrNotARepo is returned by Open when path is neither a colocated jj nor a git repo.
var ErrNotARepo = errors.New("not a repo")

// Outcome reports what Advance or PushTrunk did to the repo.
type Outcome string

const (
	// OutcomeAdvanced means trunk moved and the working copy was advanced onto it.
	OutcomeAdvanced Outcome = "advanced"
	// OutcomeUpToDate means trunk had not moved past the working copy.
	OutcomeUpToDate Outcome = "up-to-date"
	// OutcomeDiverged means local trunk and origin both moved; the advance was
	// declined and the repo left untouched.
	OutcomeDiverged Outcome = "diverged"
	// OutcomeNotDisposable means the working copy held real work and was left untouched.
	OutcomeNotDisposable Outcome = "not-disposable"
	// OutcomeRebasedGenerated means the working copy held only generated edits and was advanced onto trunk, taking upstream on conflict.
	OutcomeRebasedGenerated Outcome = "rebased-generated"
	// OutcomePushed means local trunk was strictly ahead of origin and was fast-forward pushed.
	OutcomePushed Outcome = "pushed"
	// OutcomeRaced means the repo drifted between the fetch and a mutation — git
	// HEAD moved or an operation went in-progress — so the advance was aborted
	// untouched, to retry next tick.
	OutcomeRaced Outcome = "raced"
	// OutcomeRecovered means a mid-advance user edit was snapshotted into the
	// outgoing commit; the mutation was verified, the working copy restored
	// (forward onto trunk when conflict-free, else at its original parents), and
	// no user content was lost.
	OutcomeRecovered Outcome = "recovered"
	// OutcomeBlockedUntracked means incoming trunk content would have clobbered an
	// untracked working-tree file, or a directory holding one, so the fast-forward
	// was declined and the repo left untouched.
	OutcomeBlockedUntracked Outcome = "blocked-untracked"
	// OutcomeSwept means a sweep was detected but concurrent user activity kept
	// the advance from a clean recovered state: recovery was skipped (foreign
	// ops in the window, or working-copy contention mid-recovery) or was itself
	// swept by a second mid-recovery edit. Whatever was not restored to disk
	// survives in a visible commit; no user content is lost.
	OutcomeSwept Outcome = "swept"
)

// Repo is a tracked repository whose working copy can be safely advanced onto trunk.
type Repo interface {
	// Kind reports the backing VCS, "jj" or "git".
	Kind() string
	// Origin returns the origin remote URL, or ErrNoOrigin when there is none.
	Origin(ctx context.Context) (string, error)
	// InUse reports whether the repo has in-progress work or recent activity within idle.
	InUse(ctx context.Context, idle time.Duration) (busy bool, reason string, err error)
	// LastActivity returns the most recent real activity time, or the zero time
	// (meaning unknown, never an error) when there is no activity.
	LastActivity(ctx context.Context) (time.Time, error)
	// HasTrunk reports whether a tracked origin trunk bookmark or ref exists.
	HasTrunk(ctx context.Context) (bool, error)
	// Advance fetches and safely advances the working copy onto trunk, never clobbering or pushing.
	Advance(ctx context.Context) (Outcome, error)
	// PushTrunk pushes local trunk to origin only as a clean fast-forward: it
	// reports OutcomePushed when local trunk was strictly ahead of origin/<trunk>
	// with no divergence, and OutcomeUpToDate (no error) when not ahead or diverged.
	// It does not fetch; the caller is responsible for an up-to-date origin ref.
	PushTrunk(ctx context.Context) (Outcome, error)
	// TrunkHash resolves the origin trunk commit hash.
	TrunkHash(ctx context.Context) (string, error)
}

// Open classifies the repo at path and returns a jj or git Repo. trunk defaults
// to "main" when empty. It errors when path is neither a colocated jj nor a git repo.
func Open(path, trunk string) (Repo, error) {
	if trunk == "" {
		trunk = defaultTrunk
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path %s: %w", path, err)
	}
	if isDir(filepath.Join(abs, ".jj")) {
		return &jjRepo{repoCore: repoCore{path: abs, trunk: trunk}}, nil
	}
	if isDir(filepath.Join(abs, ".git")) {
		return &gitRepo{repoCore: repoCore{path: abs, trunk: trunk}}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotARepo, abs)
}

// DetectTrunk resolves the repo's trunk from origin's default branch — the
// recorded origin/HEAD first, then a symref query against origin — falling back
// to "main". Colocated jj repos carry the same git refs, so one git probe serves
// both kinds.
func DetectTrunk(ctx context.Context, path string) string {
	if out, err := run(ctx, path, "git", "-C", path, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		// The full ref, not --short: a local branch named origin/<trunk> makes the
		// short form disambiguate to remotes/origin/<trunk>.
		if branch, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/origin/"); ok && branch != "" {
			return branch
		}
	}
	out, err := run(ctx, path, "git", "-C", path, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return defaultTrunk
	}
	if branch := parseSymrefHead(out); branch != "" {
		return branch
	}
	return defaultTrunk
}

// parseSymrefHead extracts the branch name from the `ref: refs/heads/<branch>	HEAD`
// line of `git ls-remote --symref` output.
func parseSymrefHead(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "ref:" {
			continue
		}
		return strings.TrimPrefix(fields[1], "refs/heads/")
	}
	return ""
}

// PushURLs returns where a push to origin actually lands: the configured
// pushurls, or the fetch URL when there are none, with git's own insteadOf
// rewrites applied. Colocated jj repos push through the same git remote, so one
// probe serves both kinds. It errors when the repo has no origin remote.
func PushURLs(ctx context.Context, path string) ([]string, error) {
	out, err := run(ctx, path, "git", "-C", path, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	urls := make([]string, 0, len(lines))
	for _, line := range lines {
		if u := strings.TrimSpace(line); u != "" {
			urls = append(urls, u)
		}
	}
	return urls, nil
}

// Clone clones origin into dest as a colocated jj repo (.git + .jj), regardless
// of whether origin is a jj or a plain-git source.
func Clone(ctx context.Context, origin, dest string) error {
	_, err := run(ctx, "", "jj", "git", "clone", "--colocate", origin, dest)
	return err
}

// WatchPaths returns the VCS metadata leaf directories to watch for a repo. Each
// is a small directory holding origin trunk refs (and the jj op log), cheap for
// synckit's fsnotify backend to watch recursively (kqueue/inotify, zero FSEvents
// streams) — unlike .git, whose object store would cost a descriptor per entry
// (kqueue charges files too).
// A loose-ref fetch always touches one of these; the rare packed-refs case is
// caught by the reconcile tick.
func WatchPaths(root string) []string {
	git := filepath.Join(root, ".git")
	paths := []string{
		filepath.Join(git, "refs", "remotes", "origin"),
		filepath.Join(git, "logs", "refs", "remotes", "origin"),
	}
	if isDir(filepath.Join(root, ".jj")) {
		paths = append(paths, filepath.Join(root, ".jj", "repo", "op_heads", "heads"))
	}
	return paths
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
