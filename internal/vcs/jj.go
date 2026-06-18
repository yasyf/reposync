package vcs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const jjOpTimeLayout = "2006-01-02T15:04:05-0700"

// jjOpNoise is the allow-list of operation descriptions the poller produces or
// that are not real user activity; ops matching these are ignored for InUse.
var jjOpNoise = map[string]struct{}{
	"snapshot working copy": {},
	"import git refs":       {},
	"import git head":       {},
}

type jjRepo struct {
	path  string
	trunk string
}

func (r *jjRepo) Kind() string { return "jj" }

func (r *jjRepo) Origin(ctx context.Context) (string, error) {
	return originURL(ctx, r.path)
}

func (r *jjRepo) TrunkHash(ctx context.Context) (string, error) {
	return trunkHashViaGit(ctx, r.path, r.trunk)
}

func (r *jjRepo) InUse(ctx context.Context, idle time.Duration) (bool, string, error) {
	dirty, err := r.jj(ctx, "log", "-r", "@ ~ empty()", "--no-graph", "-T", `change_id ++ "\n"`)
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(dirty) != "" {
		return true, "dirty working copy", nil
	}
	out, err := r.jj(ctx, "op", "log", "--no-graph", "--ignore-working-copy",
		"-T", `time.start().format("%Y-%m-%dT%H:%M:%S%z") ++ " | " ++ description.first_line() ++ "\n"`,
		"-n", "30")
	if err != nil {
		return false, "", err
	}
	now := time.Now()
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rawTS, rawDesc, ok := strings.Cut(line, " | ")
		if !ok {
			return false, "", fmt.Errorf("parse jj op log line %q", line)
		}
		ts := strings.TrimSpace(rawTS)
		desc := strings.TrimSpace(rawDesc)
		if _, noise := jjOpNoise[desc]; noise {
			continue
		}
		started, err := time.Parse(jjOpTimeLayout, ts)
		if err != nil {
			return false, "", fmt.Errorf("parse jj op timestamp %q: %w", ts, err)
		}
		if now.Sub(started) < idle {
			return true, "recent activity: " + desc, nil
		}
	}
	return false, "", nil
}

func (r *jjRepo) HasTrunk(ctx context.Context) (bool, error) {
	out, err := r.jj(ctx, "bookmark", "list", "--all", "--ignore-working-copy",
		"-T", `if(remote && remote != "git", name ++ "@" ++ remote ++ " tracked=" ++ tracked ++ "\n", "")`)
	if err != nil {
		return false, err
	}
	want := r.trunk + "@origin tracked=true"
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true, nil
		}
	}
	return false, nil
}

func (r *jjRepo) Advance(ctx context.Context) (Outcome, error) {
	if _, err := r.jj(ctx, "git", "fetch", "--remote", "origin", "--ignore-working-copy"); err != nil {
		return "", fmt.Errorf("jj git fetch: %w", err)
	}
	disposable, err := r.disposable(ctx)
	if err != nil {
		return "", err
	}
	if !disposable {
		return OutcomeNotDisposable, nil
	}
	moved, err := r.trunkMovedPastWorkingCopy(ctx)
	if err != nil {
		return "", err
	}
	if !moved {
		return OutcomeUpToDate, nil
	}
	if _, err := r.jj(ctx, "new", r.trunk); err != nil {
		return "", fmt.Errorf("jj new %s: %w", r.trunk, err)
	}
	return OutcomeAdvanced, nil
}

func (r *jjRepo) disposable(ctx context.Context) (bool, error) {
	out, err := r.jj(ctx, "log", "-r", "@", "--no-graph",
		"-T", `separate(" | ", "empty=" ++ empty, "desc=[" ++ description.first_line() ++ "]", "bookmarks=[" ++ bookmarks.join(",") ++ "]") ++ "\n"`)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "empty=true | desc=[] | bookmarks=[]", nil
}

func (r *jjRepo) trunkMovedPastWorkingCopy(ctx context.Context) (bool, error) {
	out, err := r.jj(ctx, "log", "-r", r.trunk+" ~ ::@", "--no-graph", "--ignore-working-copy", "-T", `"x"`)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (r *jjRepo) jj(ctx context.Context, args ...string) (string, error) {
	return run(ctx, r.path, "jj", append([]string{"--repository", r.path}, args...)...)
}
