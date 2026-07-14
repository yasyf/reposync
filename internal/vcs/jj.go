package vcs

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	jjOpTimeLayout = "2006-01-02T15:04:05-0700"
	opLogPage      = 30
	// opVerifyPage bounds the post-mutation op-log window; a pre-mutation head
	// buried deeper than this is foreign activity regardless of content.
	opVerifyPage = 8
	// jjOpSnapshot and jjOpNewEmpty pin the op descriptions reposync's own
	// mutations record; jjOwnRebaseOpPrefix embeds the rebased commit id, so
	// ownOpShape matches it by prefix.
	jjOpSnapshot        = "snapshot working copy"
	jjOpNewEmpty        = "new empty commit"
	jjOwnRebaseOpPrefix = "rebase commit "
)

// jjOpNoise is the allow-list of operation descriptions the poller produces or
// that are not real user activity; ops matching these are ignored for InUse.
var jjOpNoise = map[string]struct{}{
	jjOpSnapshot:      {},
	"import git refs": {},
	"import git head": {},
}

type jjRepo struct {
	repoCore
}

func (r *jjRepo) Kind() string { return "jj" }

// InUse first stat-checks for a live git/jj operation (opInProgress) so a locked
// repo short-circuits before any shell-out — jj blocks on working_copy.lock, so
// probing a busy repo would otherwise hang. It then never snapshots the working
// copy: every probe runs --ignore-working-copy so a concurrent interactive jj
// command (e.g. a push mid-checkout) is never raced into a "Concurrent checkout"
// failure. The recency gate runs next, and the dirty read is a heuristic over the
// last-recorded @; the authoritative stranding guards stay in Advance's true
// snapshots.
func (r *jjRepo) InUse(ctx context.Context, idle time.Duration) (bool, string, error) {
	reason, err := opInProgress(r.path)
	if err != nil {
		return false, "", err
	}
	if reason != "" {
		return true, reason, nil
	}
	started, desc, err := r.firstRealOp(ctx)
	if err != nil {
		return false, "", err
	}
	if !started.IsZero() && time.Since(started) < idle {
		return true, "recent activity: " + desc, nil
	}
	dirty, err := r.jj(ctx, "log", "-r", "@ ~ empty()", "--no-graph", "--ignore-working-copy", "-T", `change_id ++ "\n"`)
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(dirty) != "" {
		generatedOnly, _, err := r.changedPathsGeneratedOnly(ctx, true)
		if err != nil {
			return false, "", err
		}
		if !generatedOnly {
			return true, "dirty working copy", nil
		}
	}
	return false, "", nil
}

func (r *jjRepo) LastActivity(ctx context.Context) (time.Time, error) {
	started, _, err := r.firstRealOp(ctx)
	return started, err
}

// firstRealOp returns the start time and description of the most recent non-noise
// operation in the jj op log, paging in growing chunks until a real op is found or
// the log is exhausted — a burst of reposync's own noise ops (peer-notify fetch
// storms) can bury a real op arbitrarily deep. It returns the zero time and an
// empty description when the log holds only noise ops (or is empty); jjOpNoise is
// the noise set.
func (r *jjRepo) firstRealOp(ctx context.Context) (time.Time, string, error) {
	for limit := opLogPage; ; limit *= 2 {
		out, err := r.jj(ctx, "op", "log", "--no-graph", "--ignore-working-copy",
			"-T", `time.start().format("%Y-%m-%dT%H:%M:%S%z") ++ " | " ++ description.first_line() ++ "\n"`,
			"-n", strconv.Itoa(limit))
		if err != nil {
			return time.Time{}, "", err
		}
		seen := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			seen++
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
		if seen < limit {
			return time.Time{}, "", nil
		}
	}
}

// opHead returns the full id of the current operation-log head.
func (r *jjRepo) opHead(ctx context.Context) (string, error) {
	out, err := r.jj(ctx, "op", "log", "-n", "1", "--no-graph", "--ignore-working-copy", "-T", `id ++ "\n"`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// opsSince returns the descriptions of every operation recorded after sinceOp,
// newest first. ok is false when sinceOp is not among the opVerifyPage most
// recent ops — the log advanced further than our own mutation can explain, a
// foreign-activity signal for the caller, never an error.
func (r *jjRepo) opsSince(ctx context.Context, sinceOp string) (descs []string, ok bool, err error) {
	out, err := r.jj(ctx, "op", "log", "-n", strconv.Itoa(opVerifyPage), "--no-graph", "--ignore-working-copy",
		"-T", `id ++ " " ++ description.first_line() ++ "\n"`)
	if err != nil {
		return nil, false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, desc, _ := strings.Cut(line, " ")
		if id == sinceOp {
			return descs, true, nil
		}
		descs = append(descs, desc)
	}
	return nil, false, nil
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
	reason, err := opInProgress(r.path)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return OutcomeRaced, nil
	}
	if _, err := r.jj(ctx, "git", "fetch", "--remote", "origin", "--ignore-working-copy"); err != nil {
		return "", err
	}
	g, err := r.guardHead(ctx)
	if err != nil {
		return "", err
	}
	moved, err := r.trunkMovedPastWorkingCopy(ctx)
	if err != nil {
		// A conflicted trunk bookmark means local trunk and origin both moved: the
		// fetch above left it conflicted and revsets naming it now error. We cannot
		// fast-forward, so decline untouched, matching the git backend's structural
		// ahead-and-behind classification.
		if isConflictedBookmark(err) {
			return OutcomeDiverged, nil
		}
		return "", err
	}
	// Steady state: return before probeWorkingCopy snapshots @, so the daemon
	// never touches the working copy unless an advance is actually needed.
	if !moved {
		return OutcomeUpToDate, nil
	}
	// Guard before the first snapshot: probeWorkingCopy below snapshots @, and a
	// snapshot reconciles against git HEAD. A raw `git commit` between the fetch
	// and here moved HEAD with no jj op, so snapshotting would import the diverged
	// HEAD and jj new would strand it — abort untouched instead.
	ok, err := g.stable(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return OutcomeRaced, nil
	}
	p, err := r.probeWorkingCopy(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case p.empty && !p.described && !p.bookmarked:
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
		ok, err := g.stable(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return OutcomeRaced, nil
		}
		opBefore, err := r.opHead(ctx)
		if err != nil {
			return "", err
		}
		if _, err := r.jj(ctx, "new", r.trunk); err != nil {
			return "", err
		}
		survived, err := r.changeSurvived(ctx, p.changeID)
		if err != nil {
			return "", err
		}
		if !survived {
			return OutcomeAdvanced, nil
		}
		// Survival means the empty classification went stale: jj new takes its
		// own snapshot at execution time, so edits typed since the probe were
		// swept into the outgoing commit — off disk.
		return r.recoverSweptNew(ctx, p, opBefore)
	case !p.empty && !p.described && !p.bookmarked:
		generatedOnly, genPaths, err := r.changedPathsGeneratedOnly(ctx, false)
		if err != nil {
			return "", err
		}
		if !generatedOnly {
			return OutcomeNotDisposable, nil
		}
		safe, err := r.ancestrySafe(ctx)
		if err != nil {
			return "", err
		}
		if !safe {
			return OutcomeNotDisposable, nil
		}
		return r.rebaseGenerated(ctx, g, p, genPaths)
	default:
		return OutcomeNotDisposable, nil
	}
}

// recoverSweptNew restores a working-copy change that survived Advance's
// `jj new`. When the op window is exactly Advance's own op shape, the
// surviving commit is rebased onto trunk (back onto its original parents when
// that conflicts) and re-materialized as @; foreign activity keeps hands off,
// leaving the swept content preserved in a visible commit. Contention on a
// recovery mutation, or a second sweep landing in the edit's own snapshot
// window, degrades to OutcomeSwept.
func (r *jjRepo) recoverSweptNew(ctx context.Context, p wcProbe, opBefore string) (Outcome, error) {
	descs, ok, err := r.opsSince(ctx, opBefore)
	if err != nil {
		return "", err
	}
	if !ok || !ownOpShape(descs, func(d string) bool { return d == jjOpNewEmpty }) {
		return OutcomeSwept, nil
	}
	interim, err := r.workingCopyChangeID(ctx)
	if err != nil {
		return "", err
	}
	// --ignore-working-copy is safe here: the rebased commit is not @, and an
	// IWC rebase avoids snapshotting the fresh empty @ jj new left behind.
	if _, err := r.jj(ctx, "rebase", "-r", p.changeID, "-d", r.trunk, "--ignore-working-copy"); err != nil {
		return sweptOnContention(err)
	}
	conflicted, err := r.changeConflicted(ctx, p.changeID)
	if err != nil {
		return "", err
	}
	if conflicted {
		args := []string{"rebase", "-r", p.changeID}
		for _, parent := range p.parentCommitIDs {
			args = append(args, "-d", parent)
		}
		args = append(args, "--ignore-working-copy")
		if _, err := r.jj(ctx, args...); err != nil {
			return sweptOnContention(err)
		}
		conflicted, err = r.changeConflicted(ctx, p.changeID)
		if err != nil {
			return "", err
		}
		if conflicted {
			return "", fmt.Errorf("recover swept commit %s: still conflicted on its original parents", p.changeID)
		}
	}
	// No --ignore-working-copy: edit must materialize the tree; the fresh empty
	// trunk commit auto-abandons.
	if _, err := r.jj(ctx, "edit", p.changeID); err != nil {
		return sweptOnContention(err)
	}
	cur, err := r.workingCopyChangeID(ctx)
	if err != nil {
		return "", err
	}
	if cur != p.changeID {
		return "", fmt.Errorf("recover swept commit %s: @ is %s after edit", p.changeID, cur)
	}
	// The edit's own snapshot can turn the interim trunk commit non-empty: a
	// second sweep, its dirt preserved in that visible head, off-disk.
	sweptAgain, err := r.changeSurvived(ctx, interim)
	if err != nil {
		return "", err
	}
	if sweptAgain {
		return OutcomeSwept, nil
	}
	return OutcomeRecovered, nil
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
		return "", err
	}
	return OutcomePushed, nil
}

// isConflictedBookmark reports whether err is jj refusing a revset that names a
// conflicted bookmark, the expected signal that local trunk diverged from origin.
func isConflictedBookmark(err error) bool {
	return stderrContains(err, "is conflicted")
}

// IsWorkingCopyContention reports whether err is jj declining a working-copy
// update because another process raced it — transient, safe to retry next cycle.
func IsWorkingCopyContention(err error) bool {
	if err == nil {
		return false
	}
	return stderrContains(err, "Concurrent checkout") ||
		stderrContains(err, "Concurrent working copy operation") ||
		stderrContains(err, "Failed to check out commit")
}

// rebaseGenerated rebases @ (carrying only generated edits) onto trunk, then
// resolves any conflicts by taking trunk's version of each conflicted path. It
// guards on git HEAD once before the rebase; the follow-on restores complete
// reposync's own rebase, which itself moves HEAD, so re-guarding there would abort
// on our own change. The rebase takes its own snapshot at execution time, so its
// changed and conflicted paths are verified against the pre-classified generated
// set genPaths; any surplus means a user edit was swept into @ mid-advance.
func (r *jjRepo) rebaseGenerated(ctx context.Context, g *guard, p wcProbe, genPaths []string) (Outcome, error) {
	ok, err := g.stable(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return OutcomeRaced, nil
	}
	opBefore, err := r.opHead(ctx)
	if err != nil {
		return "", err
	}
	if _, err := r.jj(ctx, "rebase", "-r", "@", "-d", r.trunk); err != nil {
		return "", err
	}
	changed, err := r.changedPaths(ctx, true)
	if err != nil {
		return "", err
	}
	conflicted, err := r.conflictList(ctx)
	if err != nil {
		return "", err
	}
	gen := pathSet(genPaths)
	if !subsetOf(changed, gen) || !subsetOf(conflicted, gen) {
		return r.recoverSweptRebase(ctx, p, opBefore, conflicted)
	}
	for _, path := range conflicted {
		if _, err := r.jj(ctx, "restore", "--from", r.trunk, "--", path); err != nil {
			return "", err
		}
	}
	return OutcomeRebasedGenerated, nil
}

// recoverSweptRebase handles a rebased @ whose changed or conflicted paths
// exceed the generated set: a user edit was snapshotted into @ mid-rebase. When
// the op window is exactly the rebase's own shape, a conflict-free rebase is
// kept — the content is already on disk on the new trunk — and a conflicted one
// is undone onto its original parents, where the conflict cancels. Foreign
// activity and back-rebase contention keep hands off, leaving the rebased @
// (possibly with conflict markers) in place.
func (r *jjRepo) recoverSweptRebase(ctx context.Context, p wcProbe, opBefore string, conflicted []string) (Outcome, error) {
	descs, ok, err := r.opsSince(ctx, opBefore)
	if err != nil {
		return "", err
	}
	if !ok || !ownOpShape(descs, func(d string) bool { return strings.HasPrefix(d, jjOwnRebaseOpPrefix) }) {
		return OutcomeSwept, nil
	}
	if len(conflicted) == 0 {
		return OutcomeRecovered, nil
	}
	// No --ignore-working-copy: @ is the working copy and the tree must follow it.
	args := []string{"rebase", "-r", "@"}
	for _, parent := range p.parentCommitIDs {
		args = append(args, "-d", parent)
	}
	if _, err := r.jj(ctx, args...); err != nil {
		return sweptOnContention(err)
	}
	out, err := r.jj(ctx, "log", "-r", "@", "--no-graph", "--ignore-working-copy",
		"-T", `change_id ++ " " ++ parents.map(|c| c.commit_id()).join(",")`)
	if err != nil {
		return "", err
	}
	want := p.changeID + " " + strings.Join(p.parentCommitIDs, ",")
	if strings.TrimSpace(out) != want {
		return "", fmt.Errorf("recover swept rebase: @ is %q, want %q", strings.TrimSpace(out), want)
	}
	still, err := r.conflictList(ctx)
	if err != nil {
		return "", err
	}
	if len(still) > 0 {
		return "", fmt.Errorf("recover swept rebase: conflicts remain on original parents: %s", strings.Join(still, ", "))
	}
	return OutcomeRecovered, nil
}

// conflictList returns the conflicted paths at @ without snapshotting. jj exits
// non-zero with "No conflicts found" when there are none — an empty list, not
// an error.
func (r *jjRepo) conflictList(ctx context.Context) ([]string, error) {
	out, err := r.jj(ctx, "resolve", "--list", "--ignore-working-copy")
	if err != nil {
		if stderrContains(err, "No conflicts found") {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		paths = append(paths, conflictListPath(line))
	}
	return paths, nil
}

// jjConflictKind matches the trailing " <N>-sided conflict" marker that
// `jj resolve --list` appends after each conflicted path.
var jjConflictKind = regexp.MustCompile(`\s+\d+-sided conflict$`)

// conflictListPath recovers the full conflicted path from a `jj resolve --list`
// line by stripping the trailing conflict-kind marker, preserving spaces in the path.
func conflictListPath(line string) string {
	return strings.TrimSpace(jjConflictKind.ReplaceAllString(line, ""))
}

// changedPaths lists the paths @ changes relative to its parents.
// ignoreWorkingCopy reads the last-recorded @ without snapshotting.
func (r *jjRepo) changedPaths(ctx context.Context, ignoreWorkingCopy bool) ([]string, error) {
	args := []string{"diff", "-r", "@", "--name-only"}
	if ignoreWorkingCopy {
		args = append(args, "--ignore-working-copy")
	}
	out, err := r.jj(ctx, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// changedPathsGeneratedOnly reports whether @ changes at least one path and every
// changed path is marked linguist-generated, returning the changed set for
// post-mutation verification. ignoreWorkingCopy reads the last-recorded @
// without snapshotting, for probes that must not touch the working copy;
// mutation-gating callers pass false for a true snapshot.
func (r *jjRepo) changedPathsGeneratedOnly(ctx context.Context, ignoreWorkingCopy bool) (bool, []string, error) {
	paths, err := r.changedPaths(ctx, ignoreWorkingCopy)
	if err != nil {
		return false, nil, err
	}
	if len(paths) == 0 {
		return false, nil, nil
	}
	gen, err := generatedPaths(ctx, r.path, paths)
	if err != nil {
		return false, nil, err
	}
	return len(gen) == len(paths), paths, nil
}

// ownOpShape reports whether descs — newest first — is exactly the op shape one
// reposync mutation records: the mutating op (matched by mutOp), optionally
// preceded by the single snapshot that swept the dirt.
func ownOpShape(descs []string, mutOp func(string) bool) bool {
	switch len(descs) {
	case 1:
		return mutOp(descs[0])
	case 2:
		return mutOp(descs[0]) && descs[1] == jjOpSnapshot
	default:
		return false
	}
}

// sweptOnContention maps working-copy contention during a recovery mutation to
// OutcomeSwept — the user is acting and the swept content is already a visible
// head — and propagates every other error.
func sweptOnContention(err error) (Outcome, error) {
	if IsWorkingCopyContention(err) {
		return OutcomeSwept, nil
	}
	return "", err
}

func pathSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set
}

func subsetOf(paths []string, set map[string]struct{}) bool {
	for _, p := range paths {
		if _, ok := set[p]; !ok {
			return false
		}
	}
	return true
}

type wcProbe struct {
	empty, described, bookmarked bool
	changeID                     string
	parentCommitIDs              []string
}

// probeWorkingCopy snapshots @ and classifies it in a single read: emptiness,
// description, and bookmarks rendered as three t/f flags, plus the change id
// and parent commit ids recovery anchors on. Unparseable output is an error,
// never a guess.
func (r *jjRepo) probeWorkingCopy(ctx context.Context) (wcProbe, error) {
	out, err := r.jj(ctx, "log", "-r", "@", "--no-graph",
		"-T", `separate(" ", if(empty, "t", "f"), if(description, "t", "f"), if(bookmarks, "t", "f"), change_id, parents.map(|c| c.commit_id()).join(",")) ++ "\n"`)
	if err != nil {
		return wcProbe{}, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 5 {
		return wcProbe{}, fmt.Errorf("parse working-copy probe %q", out)
	}
	var p wcProbe
	for i, dst := range []*bool{&p.empty, &p.described, &p.bookmarked} {
		if fields[i] != "t" && fields[i] != "f" {
			return wcProbe{}, fmt.Errorf("parse working-copy probe %q", out)
		}
		*dst = fields[i] == "t"
	}
	p.changeID = fields[3]
	p.parentCommitIDs = strings.Split(fields[4], ",")
	return p, nil
}

func (r *jjRepo) workingCopyChangeID(ctx context.Context) (string, error) {
	out, err := r.jj(ctx, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// changeSurvived reports whether changeID still names a visible commit. A clean
// advance auto-abandons the empty undescribed @ it replaces, so any survival
// means the pre-mutation classification went stale.
func (r *jjRepo) changeSurvived(ctx context.Context, changeID string) (bool, error) {
	out, err := r.jj(ctx, "log", "-r", "present("+changeID+")", "--no-graph", "--ignore-working-copy", "-T", `change_id`)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (r *jjRepo) changeConflicted(ctx context.Context, changeID string) (bool, error) {
	out, err := r.jj(ctx, "log", "-r", changeID, "--no-graph", "--ignore-working-copy", "-T", `if(conflict, "t", "f")`)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "t":
		return true, nil
	case "f":
		return false, nil
	default:
		return false, fmt.Errorf("parse conflict probe %q", out)
	}
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
