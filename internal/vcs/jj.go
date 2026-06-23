package vcs

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const jjOpTimeLayout = "2006-01-02T15:04:05-0700"

const (
	// wcEmptyClean is the workingCopyState probe line for a disposable @: empty,
	// undescribed, unbookmarked.
	wcEmptyClean = "empty=true | desc=[] | bookmarks=[]"
	// wcGeneratedDirty is the workingCopyState probe line for a candidate
	// generated-only @: non-empty, undescribed, unbookmarked.
	wcGeneratedDirty = "empty=false | desc=[] | bookmarks=[]"
)

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
		generatedOnly, err := r.changedPathsGeneratedOnly(ctx)
		if err != nil {
			return false, "", err
		}
		if !generatedOnly {
			return true, "dirty working copy", nil
		}
	}
	started, desc, err := r.firstRealOp(ctx)
	if err != nil {
		return false, "", err
	}
	if !started.IsZero() && time.Since(started) < idle {
		return true, "recent activity: " + desc, nil
	}
	return false, "", nil
}

func (r *jjRepo) LastActivity(ctx context.Context) (time.Time, error) {
	started, _, err := r.firstRealOp(ctx)
	return started, err
}

// firstRealOp returns the start time and description of the most recent non-noise
// operation in the jj op log. It returns the zero time and an empty description
// when the log holds only noise ops (or is empty); jjOpNoise is the noise set.
func (r *jjRepo) firstRealOp(ctx context.Context) (time.Time, string, error) {
	out, err := r.jj(ctx, "op", "log", "--no-graph", "--ignore-working-copy",
		"-T", `time.start().format("%Y-%m-%dT%H:%M:%S%z") ++ " | " ++ description.first_line() ++ "\n"`,
		"-n", "30")
	if err != nil {
		return time.Time{}, "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rawTS, rawDesc, ok := strings.Cut(line, " | ")
		if !ok {
			return time.Time{}, "", fmt.Errorf("parse jj op log line %q", line)
		}
		ts := strings.TrimSpace(rawTS)
		desc := strings.TrimSpace(rawDesc)
		if _, noise := jjOpNoise[desc]; noise {
			continue
		}
		started, err := time.Parse(jjOpTimeLayout, ts)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("parse jj op timestamp %q: %w", ts, err)
		}
		return started, desc, nil
	}
	return time.Time{}, "", nil
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
	moved, err := r.trunkMovedPastWorkingCopy(ctx)
	if err != nil {
		// A conflicted trunk bookmark means local trunk diverged from origin: the
		// fetch above left it conflicted and revsets naming it now error. We cannot
		// fast-forward, so decline quietly like the git backend does on a non-FF.
		if isConflictedBookmark(err) {
			return OutcomeUpToDate, nil
		}
		return "", err
	}
	disposable, err := r.disposable(ctx)
	if err != nil {
		return "", err
	}
	if disposable {
		if !moved {
			return OutcomeUpToDate, nil
		}
		// An empty @ is only safe to advance when its parent is already on trunk.
		// Sitting atop unpushed local work, jj new <trunk> would reparent the
		// working copy onto trunk and strand that work — leave it untouched.
		safe, err := r.ancestrySafe(ctx)
		if err != nil {
			return "", err
		}
		if !safe {
			return OutcomeNotDisposable, nil
		}
		if _, err := r.jj(ctx, "new", r.trunk); err != nil {
			return "", fmt.Errorf("jj new %s: %w", r.trunk, err)
		}
		return OutcomeAdvanced, nil
	}
	generatedOnly, err := r.generatedOnlyDirty(ctx)
	if err != nil {
		return "", err
	}
	if generatedOnly {
		safe, err := r.ancestrySafe(ctx)
		if err != nil {
			return "", err
		}
		if safe {
			if !moved {
				return OutcomeUpToDate, nil
			}
			return r.rebaseGenerated(ctx)
		}
	}
	return OutcomeNotDisposable, nil
}

// PushTrunk fast-forward pushes the local <trunk> bookmark to origin. It pushes
// only when local trunk is strictly ahead of <trunk>@origin with no divergence;
// not-ahead or diverged returns OutcomeUpToDate without pushing. Detection is
// bookmark-relative (never @), so an empty post-Advance @ is ignored.
//
// On true divergence `jj git fetch` leaves the local bookmark conflicted, and any
// revset naming the bare bookmark errors ("Name `<trunk>` is conflicted"). That
// is an expected non-fast-forwardable condition, treated as OutcomeUpToDate.
func (r *jjRepo) PushTrunk(ctx context.Context) (Outcome, error) {
	ahead, err := r.jj(ctx, "log", "-r", r.trunk+"@origin.."+r.trunk, "--no-graph", "--ignore-working-copy", "-T", `"x"`)
	if err != nil {
		if isConflictedBookmark(err) {
			return OutcomeUpToDate, nil
		}
		return "", err
	}
	if strings.TrimSpace(ahead) == "" {
		return OutcomeUpToDate, nil
	}
	diverged, err := r.jj(ctx, "log", "-r", r.trunk+"@origin ~ ::"+r.trunk, "--no-graph", "--ignore-working-copy", "-T", `"x"`)
	if err != nil {
		if isConflictedBookmark(err) {
			return OutcomeUpToDate, nil
		}
		return "", err
	}
	if strings.TrimSpace(diverged) != "" {
		return OutcomeUpToDate, nil
	}
	if _, err := r.jj(ctx, "git", "push", "--remote", "origin", "--bookmark", r.trunk, "--ignore-working-copy"); err != nil {
		return "", fmt.Errorf("jj git push %s: %w", r.trunk, err)
	}
	return OutcomePushed, nil
}

// isConflictedBookmark reports whether err is jj refusing a revset that names a
// conflicted bookmark, the expected signal that local trunk diverged from origin.
func isConflictedBookmark(err error) bool {
	return strings.Contains(err.Error(), "is conflicted")
}

// rebaseGenerated rebases @ (carrying only generated edits) onto trunk, then
// resolves any conflicts by taking trunk's version of each conflicted path.
func (r *jjRepo) rebaseGenerated(ctx context.Context) (Outcome, error) {
	if _, err := r.jj(ctx, "rebase", "-r", "@", "-d", r.trunk); err != nil {
		return "", fmt.Errorf("jj rebase: %w", err)
	}
	conflicts, err := r.jj(ctx, "resolve", "--list")
	if err != nil {
		if strings.Contains(err.Error(), "No conflicts found") {
			return OutcomeRebasedGenerated, nil
		}
		return "", fmt.Errorf("jj resolve --list: %w", err)
	}
	for _, line := range strings.Split(conflicts, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		path := conflictListPath(line)
		if _, err := r.jj(ctx, "restore", "--from", r.trunk, "--", path); err != nil {
			return "", fmt.Errorf("jj restore %s: %w", path, err)
		}
	}
	return OutcomeRebasedGenerated, nil
}

// jjConflictKind matches the trailing " <N>-sided conflict" marker that
// `jj resolve --list` appends after each conflicted path.
var jjConflictKind = regexp.MustCompile(`\s+\d+-sided conflict$`)

// conflictListPath recovers the full conflicted path from a `jj resolve --list`
// line by stripping the trailing conflict-kind marker, preserving spaces in the path.
func conflictListPath(line string) string {
	return strings.TrimSpace(jjConflictKind.ReplaceAllString(line, ""))
}

// changedPathsGeneratedOnly reports whether @ changes at least one path and every
// changed path is marked linguist-generated.
func (r *jjRepo) changedPathsGeneratedOnly(ctx context.Context) (bool, error) {
	out, err := r.jj(ctx, "diff", "-r", "@", "--name-only")
	if err != nil {
		return false, fmt.Errorf("jj diff: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return false, nil
	}
	gen, err := generatedPaths(ctx, r.path, paths)
	if err != nil {
		return false, err
	}
	return len(gen) == len(paths), nil
}

// workingCopyState renders @'s emptiness, description, and bookmarks as a single
// trimmed probe line, compared against wcEmptyClean / wcGeneratedDirty.
func (r *jjRepo) workingCopyState(ctx context.Context) (string, error) {
	out, err := r.jj(ctx, "log", "-r", "@", "--no-graph",
		"-T", `separate(" | ", "empty=" ++ empty, "desc=[" ++ description.first_line() ++ "]", "bookmarks=[" ++ bookmarks.join(",") ++ "]") ++ "\n"`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// generatedOnlyDirty reports whether @ holds only generated edits: non-empty, no
// description, no bookmarks, and every changed path is generated.
func (r *jjRepo) generatedOnlyDirty(ctx context.Context) (bool, error) {
	state, err := r.workingCopyState(ctx)
	if err != nil {
		return false, err
	}
	if state != wcGeneratedDirty {
		return false, nil
	}
	return r.changedPathsGeneratedOnly(ctx)
}

func (r *jjRepo) disposable(ctx context.Context) (bool, error) {
	state, err := r.workingCopyState(ctx)
	if err != nil {
		return false, err
	}
	return state == wcEmptyClean, nil
}

// ancestrySafe reports whether every parent of @ is already contained in trunk.
// A non-empty `@- ~ ::<trunk>` result means @ sits atop an unpushed local commit,
// so rebasing @ would strand that work — unsafe.
func (r *jjRepo) ancestrySafe(ctx context.Context) (bool, error) {
	out, err := r.jj(ctx, "log", "-r", "@- ~ ::"+r.trunk, "--no-graph", "--ignore-working-copy", "-T", `"x"`)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
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
