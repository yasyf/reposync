package vcs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const gitReflogTimeLayout = "2006-01-02 15:04:05 -0700"

type gitRepo struct {
	path  string
	trunk string
}

func (r *gitRepo) Kind() string { return "git" }

func (r *gitRepo) Origin(ctx context.Context) (string, error) {
	return originURL(ctx, r.path)
}

func (r *gitRepo) TrunkHash(ctx context.Context) (string, error) {
	return trunkHashViaGit(ctx, r.path, r.trunk)
}

func (r *gitRepo) InUse(ctx context.Context, idle time.Duration) (bool, string, error) {
	status, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(status) != "" {
		return true, "dirty working tree", nil
	}
	reflog, err := r.git(ctx, "reflog", "--date=iso", "-n", "1")
	if err != nil {
		return false, "", err
	}
	recent, err := r.reflogRecent(strings.TrimSpace(reflog), idle)
	if err != nil {
		return false, "", err
	}
	if recent {
		return true, "recent activity", nil
	}
	return false, "", nil
}

func (r *gitRepo) HasTrunk(ctx context.Context) (bool, error) {
	if _, err := r.git(ctx, "rev-parse", "--verify", "-q", "origin/"+r.trunk); err != nil {
		return false, nil
	}
	return true, nil
}

func (r *gitRepo) Advance(ctx context.Context) (Outcome, error) {
	if _, err := r.git(ctx, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("git fetch: %w", err)
	}
	behind, err := r.behindCount(ctx)
	if err != nil {
		return "", err
	}
	branch, err := r.currentBranch(ctx)
	if err != nil {
		return "", err
	}
	if branch == r.trunk {
		if _, err := r.git(ctx, "merge", "--ff-only", "origin/"+r.trunk); err != nil {
			return OutcomeUpToDate, nil
		}
		if behind > 0 {
			return OutcomeAdvanced, nil
		}
		return OutcomeUpToDate, nil
	}
	if _, err := r.git(ctx, "fetch", "origin", r.trunk+":"+r.trunk); err != nil {
		return OutcomeUpToDate, nil
	}
	if behind > 0 {
		return OutcomeAdvanced, nil
	}
	return OutcomeUpToDate, nil
}

func (r *gitRepo) behindCount(ctx context.Context) (int, error) {
	out, err := r.git(ctx, "rev-list", "--left-right", "--count", r.trunk+"...origin/"+r.trunk)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, fmt.Errorf("parse rev-list count %q", out)
	}
	behind, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("parse behind count %q: %w", fields[1], err)
	}
	return behind, nil
}

func (r *gitRepo) currentBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "symbolic-ref", "--short", "-q", "HEAD")
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	// symbolic-ref -q fails on a detached HEAD; confirm HEAD resolves to tell a
	// detached HEAD (a legitimate empty branch) from a real failure.
	if _, headErr := r.git(ctx, "rev-parse", "--verify", "-q", "HEAD"); headErr == nil {
		return "", nil
	}
	return "", fmt.Errorf("resolve current branch: %w", err)
}

func (r *gitRepo) reflogRecent(line string, idle time.Duration) (bool, error) {
	open := strings.Index(line, "@{")
	if open < 0 {
		return false, nil
	}
	rest := line[open+2:]
	end := strings.Index(rest, "}")
	if end < 0 {
		return false, fmt.Errorf("parse reflog timestamp %q", line)
	}
	at, err := time.Parse(gitReflogTimeLayout, rest[:end])
	if err != nil {
		return false, fmt.Errorf("parse reflog timestamp %q: %w", rest[:end], err)
	}
	return time.Since(at) < idle, nil
}

func (r *gitRepo) git(ctx context.Context, args ...string) (string, error) {
	return run(ctx, r.path, "git", append([]string{"-C", r.path}, args...)...)
}
