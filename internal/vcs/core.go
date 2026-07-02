package vcs

import (
	"context"
	"strings"
)

// repoCore is the git-backing substrate shared by both backends: a colocated jj
// repo answers these questions through the same git commands as a plain git repo.
type repoCore struct {
	path  string
	trunk string
}

// Origin resolves the origin remote via git, which works for both jj and git repos.
func (c *repoCore) Origin(ctx context.Context) (string, error) {
	out, err := c.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		if stderrContains(err, "No such remote") {
			return "", ErrNoOrigin
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// TrunkHash resolves origin/<trunk> through the colocated or plain git backing.
func (c *repoCore) TrunkHash(ctx context.Context) (string, error) {
	out, err := c.git(ctx, "rev-parse", "refs/remotes/origin/"+c.trunk)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// headHash resolves the current git HEAD commit through the colocated or plain
// git backing. In a colocated jj repo HEAD moves on a raw `git commit`, which
// creates no jj operation — the drift signal jj's own op log cannot see, and the
// anchor guardHead captures for Advance to re-check before every mutation and
// abort a raced advance.
func (c *repoCore) headHash(ctx context.Context) (string, error) {
	out, err := c.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *repoCore) git(ctx context.Context, args ...string) (string, error) {
	return run(ctx, c.path, "git", append([]string{"-C", c.path}, args...)...)
}

// guard anchors an advance to the git HEAD its fetch observed.
type guard struct {
	core *repoCore
	head string
}

func (c *repoCore) guardHead(ctx context.Context) (*guard, error) {
	head, err := c.headHash(ctx)
	if err != nil {
		return nil, err
	}
	return &guard{core: c, head: head}, nil
}

// stable reports whether git HEAD still matches the captured head and no git/jj
// operation is now in progress — the pre-mutation guard that aborts an advance
// the instant the user's state drifts from what the fetch observed. A raw
// `git commit` moves HEAD; a live git or jj command drops a lock file. Either
// makes advancing the working copy unsafe. In a colocated repo the raw commit
// records no jj op, so a jj snapshot would silently reconcile @ against the
// diverged HEAD and jj new would strand the commit; git HEAD is the drift signal
// jj's own op log cannot provide, and — unlike the op head — is never perturbed
// by reposync's own snapshots, so it never false-aborts.
func (g *guard) stable(ctx context.Context) (bool, error) {
	reason, err := opInProgress(g.core.path)
	if err != nil {
		return false, err
	}
	if reason != "" {
		return false, nil
	}
	now, err := g.core.headHash(ctx)
	if err != nil {
		return false, err
	}
	return now == g.head, nil
}
