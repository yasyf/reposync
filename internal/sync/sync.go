// Package sync reconciles a local repository with one of its git remotes.
package sync

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/yasyf/reposync/internal/config"
)

// State is the outcome of syncing a single repository.
type State string

const (
	// StateUpToDate means the local branch already matched the remote.
	StateUpToDate State = "up-to-date"
	// StatePulled means the local branch was fast-forwarded from the remote.
	StatePulled State = "pulled"
	// StatePushed means local commits were pushed to the remote.
	StatePushed State = "pushed"
	// StateDiverged means the branch had both ahead and behind commits; skipped.
	StateDiverged State = "diverged"
)

// Result reports what happened to one repository.
type Result struct {
	Repo  config.Repo
	State State
	Err   error
}

// runner runs git commands; swapped out in tests against real temp repos.
type runner func(ctx context.Context, dir string, args ...string) (string, error)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// All syncs every repo in the config, returning one Result per repo.
func All(ctx context.Context, cfg *config.Config) []Result {
	results := make([]Result, len(cfg.Repos))
	for i, repo := range cfg.Repos {
		results[i] = syncRepo(ctx, repo, git)
	}
	return results
}

func syncRepo(ctx context.Context, repo config.Repo, run runner) Result {
	res := Result{Repo: repo}

	if _, err := run(ctx, repo.Path, "rev-parse", "--git-dir"); err != nil {
		res.Err = fmt.Errorf("%s is not a git repository: %w", repo.Path, err)
		return res
	}

	if repo.AutoCommit {
		if err := autoCommit(ctx, repo.Path, run); err != nil {
			res.Err = err
			return res
		}
	}

	branch := repo.Branch
	if branch == "" {
		current, err := run(ctx, repo.Path, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			res.Err = err
			return res
		}
		branch = current
	}

	if _, err := run(ctx, repo.Path, "fetch", repo.Remote, branch); err != nil {
		res.Err = err
		return res
	}

	local := fmt.Sprintf("refs/heads/%s", branch)
	remote := fmt.Sprintf("refs/remotes/%s/%s", repo.Remote, branch)

	ahead, behind, err := revCounts(ctx, repo.Path, local, remote, run)
	if err != nil {
		res.Err = err
		return res
	}

	switch {
	case ahead == 0 && behind == 0:
		res.State = StateUpToDate
	case ahead == 0 && behind > 0:
		if _, err := run(ctx, repo.Path, "merge", "--ff-only", remote); err != nil {
			res.Err = err
			return res
		}
		res.State = StatePulled
	case ahead > 0 && behind == 0:
		if _, err := run(ctx, repo.Path, "push", repo.Remote, branch); err != nil {
			res.Err = err
			return res
		}
		res.State = StatePushed
	default:
		res.State = StateDiverged
		res.Err = fmt.Errorf("%s: branch %s diverged (%d ahead, %d behind); resolve manually", repo.Path, branch, ahead, behind)
	}

	return res
}

func autoCommit(ctx context.Context, dir string, run runner) error {
	status, err := run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	if _, err := run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err = run(ctx, dir, "commit", "-m", "reposync: auto-commit")
	return err
}

func revCounts(ctx context.Context, dir, local, remote string, run runner) (ahead, behind int, err error) {
	out, err := run(ctx, dir, "rev-list", "--left-right", "--count", local+"..."+remote)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse ahead count %q: %w", fields[0], err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse behind count %q: %w", fields[1], err)
	}
	return ahead, behind, nil
}
