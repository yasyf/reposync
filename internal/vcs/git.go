package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const gitReflogTimeLayout = "2006-01-02 15:04:05 -0700"

type gitRepo struct {
	repoCore
}

func (r *gitRepo) Kind() string { return "git" }

func (r *gitRepo) InUse(ctx context.Context, idle time.Duration) (bool, string, error) {
	reason, err := opInProgress(r.path)
	if err != nil {
		return false, "", err
	}
	if reason != "" {
		return true, reason, nil
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
	clean, generatedOnly, _, err := dirtState(ctx, r.path)
	if err != nil {
		return false, "", err
	}
	if !clean && !generatedOnly {
		return true, "dirty working tree", nil
	}
	return false, "", nil
}

func (r *gitRepo) LastActivity(ctx context.Context) (time.Time, error) {
	reflog, err := r.git(ctx, "reflog", "--date=iso", "-n", "1")
	if err != nil {
		return time.Time{}, err
	}
	return parseReflogTime(strings.TrimSpace(reflog))
}

func (r *gitRepo) HasTrunk(ctx context.Context) (bool, error) {
	if _, err := r.git(ctx, "rev-parse", "--verify", "-q", "origin/"+r.trunk); err != nil {
		var cerr *cmdError
		if errors.As(err, &cerr) && cerr.code == 1 && cerr.stderr == "" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *gitRepo) Advance(ctx context.Context) (Outcome, error) {
	reason, err := opInProgress(r.path)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return OutcomeRaced, nil
	}
	if _, err := r.git(ctx, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("git fetch: %w", err)
	}
	g, err := r.guardHead(ctx)
	if err != nil {
		return "", err
	}
	ahead, behind, err := r.aheadBehind(ctx)
	if err != nil {
		return "", err
	}
	onTrunk, err := r.onTrunk(ctx)
	if err != nil {
		return "", err
	}
	if onTrunk {
		_, generatedOnly, generated, err := dirtState(ctx, r.path)
		if err != nil {
			return "", err
		}
		if generatedOnly {
			return r.advanceGenerated(ctx, g, ahead, behind, generated)
		}
		if behind == 0 {
			return OutcomeUpToDate, nil
		}
		if ahead > 0 {
			return OutcomeDiverged, nil
		}
		ok, err := g.stable(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return OutcomeRaced, nil
		}
		if _, err := r.git(ctx, "merge", "--ff-only", "origin/"+r.trunk); err != nil {
			return "", fmt.Errorf("git merge --ff-only: %w", err)
		}
		return OutcomeAdvanced, nil
	}
	if behind == 0 {
		return OutcomeUpToDate, nil
	}
	if ahead > 0 {
		return OutcomeDiverged, nil
	}
	// Off trunk, the working copy is not involved: fast-forward the local <trunk>
	// ref from the tracking ref already fetched above. The plus-less refspec is
	// the fast-forward guard — never force it.
	if _, err := r.git(ctx, "fetch", ".", "refs/remotes/origin/"+r.trunk+":refs/heads/"+r.trunk); err != nil {
		return "", fmt.Errorf("git fetch %s: %w", r.trunk, err)
	}
	return OutcomeAdvanced, nil
}

// advanceGenerated advances an on-trunk working tree whose only uncommitted edits
// are to generated files. Generated edits that conflict with what trunk changes
// are dropped (upstream wins); cleanly-applying generated edits are carried
// untouched through the fast-forward. A diverged trunk is declined before the
// restore loop touches any local edit — the merge below could not fast-forward it.
func (r *gitRepo) advanceGenerated(ctx context.Context, g *guard, ahead, behind int, generated []string) (Outcome, error) {
	if behind == 0 {
		return OutcomeUpToDate, nil
	}
	if ahead > 0 {
		return OutcomeDiverged, nil
	}
	changed, err := r.trunkChangedPaths(ctx)
	if err != nil {
		return "", err
	}
	ok, err := g.stable(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return OutcomeRaced, nil
	}
	for _, p := range generated {
		if _, ok := changed[p]; !ok {
			continue
		}
		tracked, err := r.tracked(ctx, p)
		if err != nil {
			return "", err
		}
		if tracked {
			if _, err := r.git(ctx, "restore", "--staged", "--worktree", "--source=HEAD", "--", p); err != nil {
				return "", fmt.Errorf("git restore %s: %w", p, err)
			}
			continue
		}
		if err := os.Remove(filepath.Join(r.path, p)); err != nil {
			return "", fmt.Errorf("remove untracked generated %s: %w", p, err)
		}
	}
	if _, err := r.git(ctx, "merge", "--ff-only", "origin/"+r.trunk); err != nil {
		return "", fmt.Errorf("git merge --ff-only: %w", err)
	}
	return OutcomeRebasedGenerated, nil
}

// trunkChangedPaths returns the set of paths that differ between HEAD and origin/<trunk>.
func (r *gitRepo) trunkChangedPaths(ctx context.Context) (map[string]struct{}, error) {
	out, err := r.git(ctx, "diff", "--name-only", "HEAD", "origin/"+r.trunk)
	if err != nil {
		return nil, fmt.Errorf("git diff trunk: %w", err)
	}
	changed := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			changed[p] = struct{}{}
		}
	}
	return changed, nil
}

// tracked reports whether path is tracked in the index.
func (r *gitRepo) tracked(ctx context.Context, path string) (bool, error) {
	out, err := r.git(ctx, "ls-files", "--error-unmatch", "--", path)
	if err != nil {
		if exitCode(err) == 1 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// PushTrunk fast-forward pushes local <trunk> to origin. It pushes only when
// local trunk is strictly ahead of origin/<trunk> with no divergence; not-ahead
// or diverged returns OutcomeUpToDate without pushing. It operates on the local
// <trunk> ref, so it no-ops when checked out on a feature branch.
func (r *gitRepo) PushTrunk(ctx context.Context) (Outcome, error) {
	ahead, behind, err := r.aheadBehind(ctx)
	if err != nil {
		return "", err
	}
	if ahead == 0 {
		return OutcomeUpToDate, nil
	}
	if behind > 0 {
		return OutcomeUpToDate, nil
	}
	if _, err := r.git(ctx, "push", "origin", r.trunk+":"+r.trunk); err != nil {
		return "", fmt.Errorf("git push %s: %w", r.trunk, err)
	}
	return OutcomePushed, nil
}

// aheadBehind counts how many commits local <trunk> is ahead of and behind
// origin/<trunk>, parsing the tab-separated "ahead\tbehind" rev-list output.
func (r *gitRepo) aheadBehind(ctx context.Context) (ahead, behind int, err error) {
	out, err := r.git(ctx, "rev-list", "--left-right", "--count", r.trunk+"...origin/"+r.trunk)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("parse rev-list count %q", out)
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

func (r *gitRepo) onTrunk(ctx context.Context) (bool, error) {
	out, err := r.git(ctx, "symbolic-ref", "--short", "-q", "HEAD")
	if err == nil {
		return strings.TrimSpace(out) == r.trunk, nil
	}
	// symbolic-ref -q fails on a detached HEAD; confirm HEAD resolves to tell a
	// detached HEAD (legitimately off trunk) from a real failure.
	if _, headErr := r.git(ctx, "rev-parse", "--verify", "-q", "HEAD"); headErr == nil {
		return false, nil
	}
	return false, fmt.Errorf("resolve current branch: %w", err)
}

func (r *gitRepo) reflogRecent(line string, idle time.Duration) (bool, error) {
	at, err := parseReflogTime(line)
	if err != nil {
		return false, err
	}
	if at.IsZero() {
		return false, nil
	}
	return time.Since(at) < idle, nil
}

// parseReflogTime extracts the timestamp from a `git reflog --date=iso` line. It
// returns the zero time and a nil error when the line carries no `@{...}` stamp
// (an empty reflog), and an error only when a present stamp fails to parse.
func parseReflogTime(line string) (time.Time, error) {
	open := strings.Index(line, "@{")
	if open < 0 {
		return time.Time{}, nil
	}
	rest := line[open+2:]
	end := strings.Index(rest, "}")
	if end < 0 {
		return time.Time{}, fmt.Errorf("parse reflog timestamp %q", line)
	}
	at, err := time.Parse(gitReflogTimeLayout, rest[:end])
	if err != nil {
		return time.Time{}, fmt.Errorf("parse reflog timestamp %q: %w", rest[:end], err)
	}
	return at, nil
}
