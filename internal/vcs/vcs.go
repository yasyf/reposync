// Package vcs is the verified jj+git command layer that keeps a tracked repo on
// the latest trunk without ever clobbering in-progress work and without ever
// pushing. A repo is colocated jj when a .jj dir exists at its root, otherwise
// plain git when .git exists.
package vcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultTrunk = "main"

// ErrNoOrigin is returned by Origin when the repo has no origin remote.
var ErrNoOrigin = errors.New("no origin remote")

// ErrNotARepo is returned by Open when path is neither a colocated jj nor a git repo.
var ErrNotARepo = errors.New("not a repo")

// Outcome reports what Advance did to the working copy.
type Outcome string

const (
	// OutcomeAdvanced means trunk moved and the working copy was advanced onto it.
	OutcomeAdvanced Outcome = "advanced"
	// OutcomeUpToDate means trunk had not moved past the working copy.
	OutcomeUpToDate Outcome = "up-to-date"
	// OutcomeBusy means the repo was in use and was left untouched.
	OutcomeBusy Outcome = "busy"
	// OutcomeNoTrunk means no tracked origin trunk exists to advance onto.
	OutcomeNoTrunk Outcome = "no-trunk"
	// OutcomeNotDisposable means the working copy held real work and was left untouched.
	OutcomeNotDisposable Outcome = "not-disposable"
	// OutcomeRebasedGenerated means the working copy held only generated edits and was advanced onto trunk, taking upstream on conflict.
	OutcomeRebasedGenerated Outcome = "rebased-generated"
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
		return &jjRepo{path: abs, trunk: trunk}, nil
	}
	if isDir(filepath.Join(abs, ".git")) {
		return &gitRepo{path: abs, trunk: trunk}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotARepo, abs)
}

// Clone clones origin into dest as a colocated jj repo (.git + .jj), regardless
// of whether origin is a jj or a plain-git source.
func Clone(ctx context.Context, origin, dest string) error {
	if _, err := run(ctx, "", "jj", "git", "clone", "--colocate", origin, dest); err != nil {
		return fmt.Errorf("jj git clone %s: %w", origin, err)
	}
	return nil
}

// originURL resolves the origin remote via git, which works for both jj and git repos.
func originURL(ctx context.Context, path string) (string, error) {
	out, err := run(ctx, path, "git", "-C", path, "remote", "get-url", "origin")
	if err != nil {
		if strings.Contains(err.Error(), "No such remote") {
			return "", ErrNoOrigin
		}
		return "", fmt.Errorf("get origin url: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// trunkHashViaGit resolves origin/<trunk> through the colocated or plain git backing.
func trunkHashViaGit(ctx context.Context, path, trunk string) (string, error) {
	out, err := run(ctx, path, "git", "-C", path, "rev-parse", "refs/remotes/origin/"+trunk)
	if err != nil {
		return "", fmt.Errorf("rev-parse origin/%s: %w", trunk, err)
	}
	return strings.TrimSpace(out), nil
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runStdin(ctx, dir, "", name, args...)
}

func runStdin(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
