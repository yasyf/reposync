package vcs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openGit(t *testing.T, path string) Repo {
	t.Helper()
	r, err := Open(path, "main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r.Kind() != "git" {
		t.Fatalf("kind = %q, want git", r.Kind())
	}
	return r
}

func TestGitOpenAndOrigin(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	origin, err := r.Origin(context.Background())
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	if origin != f.origin {
		t.Fatalf("origin = %q, want %q", origin, f.origin)
	}
	hash, err := r.TrunkHash(context.Background())
	if err != nil {
		t.Fatalf("trunk hash: %v", err)
	}
	if hash != f.originMain() {
		t.Fatalf("trunk hash = %q, want %q", hash, f.originMain())
	}
}

func TestGitHasTrunk(t *testing.T) {
	f := newFixture(t)
	r := openGit(t, f.gitClone(filepath.Join(f.root, "clone")))
	ok, err := r.HasTrunk(context.Background())
	if err != nil {
		t.Fatalf("has trunk: %v", err)
	}
	if !ok {
		t.Fatal("HasTrunk = false, want true")
	}
}

func TestGitAdvance(t *testing.T) {
	t.Run("clean up-to-date returns up-to-date", func(t *testing.T) {
		f := newFixture(t)
		r := openGit(t, f.gitClone(filepath.Join(f.root, "clone")))
		got, err := r.Advance(context.Background())
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if got != OutcomeUpToDate {
			t.Fatalf("outcome = %q, want up-to-date", got)
		}
	})

	t.Run("on trunk behind advances via ff", func(t *testing.T) {
		f := newFixture(t)
		dest := f.gitClone(filepath.Join(f.root, "clone"))
		r := openGit(t, dest)
		want := f.advanceOrigin("v2")

		got, err := r.Advance(context.Background())
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if got != OutcomeAdvanced {
			t.Fatalf("outcome = %q, want advanced", got)
		}
		localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main"))
		if localMain != want {
			t.Fatalf("local main = %q, want origin %q", localMain, want)
		}
	})
}

// TestGitStableDetectsDrift proves the pre-mutation guard predicate: stable holds
// on a quiet repo, and reports drift the instant a live-operation lock appears or
// git HEAD moves out from under the captured hash (a raw commit).
func TestGitStableDetectsDrift(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest).(*gitRepo)
	ctx := context.Background()

	g, err := r.guardHead(ctx)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	ok, err := g.stable(ctx)
	if err != nil {
		t.Fatalf("stable: %v", err)
	}
	if !ok {
		t.Fatal("stable = false on a quiet repo, want true")
	}

	lock := filepath.Join(dest, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	ok, err = g.stable(ctx)
	if err != nil {
		t.Fatalf("stable (locked): %v", err)
	}
	if ok {
		t.Fatal("stable = true with index.lock held, want false")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove lock: %v", err)
	}

	f.writeFile(dest, "raw.txt", "raw commit\n")
	f.runGit(dest, "add", "raw.txt")
	f.runGit(dest, "commit", "-qm", "raw user commit")
	ok, err = g.stable(ctx)
	if err != nil {
		t.Fatalf("stable (moved): %v", err)
	}
	if ok {
		t.Fatal("stable = true after HEAD moved out from the captured hash, want false")
	}
}

// TestGitAdvanceAbortsUnderLock proves the fetch gate: a live-operation lock at the
// start of Advance yields OutcomeRaced without fetching or moving HEAD, even though
// trunk moved and a clean advance would otherwise fast-forward.
func TestGitAdvanceAbortsUnderLock(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.advanceOrigin("v2")

	headBefore := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD"))
	trackingBefore := strings.TrimSpace(f.runGit(dest, "rev-parse", "refs/remotes/origin/main"))

	lock := filepath.Join(dest, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRaced {
		t.Fatalf("outcome = %q, want raced", got)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	if head := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD")); head != headBefore {
		t.Fatalf("HEAD moved to %q, want unchanged %q", head, headBefore)
	}
	if tracking := strings.TrimSpace(f.runGit(dest, "rev-parse", "refs/remotes/origin/main")); tracking != trackingBefore {
		t.Fatalf("origin/main tracking ref moved to %q, want unchanged %q (fetch must be gated)", tracking, trackingBefore)
	}
}

func TestGitInUseDirty(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.writeFile(dest, "DIRTY.txt", "uncommitted\n")

	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on dirty tree")
	}
	if reason != "dirty working tree" {
		t.Fatalf("reason = %q, want dirty working tree", reason)
	}

	// Advance must leave the dirty file intact (it can ff-merge a clean index,
	// but the caller gates on InUse; here we assert the file survives regardless).
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !f.fileExists(dest, "DIRTY.txt") {
		t.Fatal("dirty file was clobbered")
	}
	if got := f.readFile(dest, "DIRTY.txt"); got != "uncommitted\n" {
		t.Fatalf("dirty file content changed to %q", got)
	}
}

func TestGitInUseRecentReflog(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy from recent clone reflog entry")
	}
	if reason != "recent activity" {
		t.Fatalf("reason = %q, want recent activity", reason)
	}

	notBusy, _, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if notBusy {
		t.Fatal("InUse = busy with tiny idle window, want not busy")
	}
}

func TestGitLastActivity(t *testing.T) {
	t.Run("recent after clone", func(t *testing.T) {
		f := newFixture(t)
		r := openGit(t, f.gitClone(filepath.Join(f.root, "clone")))

		got, err := r.LastActivity(context.Background())
		if err != nil {
			t.Fatalf("last activity: %v", err)
		}
		if got.IsZero() {
			t.Fatal("LastActivity = zero, want a recent clone reflog time")
		}
		if since := time.Since(got); since > time.Hour {
			t.Fatalf("LastActivity = %v (%v ago), want within the last hour", got, since)
		}
	})

	t.Run("zero on empty reflog", func(t *testing.T) {
		f := newFixture(t)
		dest := f.gitClone(filepath.Join(f.root, "clone"))
		r := openGit(t, dest)
		f.runGit(dest, "reflog", "expire", "--expire=now", "--all")

		got, err := r.LastActivity(context.Background())
		if err != nil {
			t.Fatalf("last activity: %v", err)
		}
		if !got.IsZero() {
			t.Fatalf("LastActivity = %v, want zero on an empty reflog", got)
		}
	})
}

func TestGitDivergedRefusesFFNoClobber(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Divergent local commit on main.
	f.writeFile(dest, "LOCAL.md", "local-only\n")
	f.runGit(dest, "add", "LOCAL.md")
	f.runGit(dest, "commit", "-qm", "local divergent")
	localHead := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD"))
	f.advanceOrigin("v2")

	originBefore := f.originMain()
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date (ff refused, no-op)", got)
	}
	if head := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD")); head != localHead {
		t.Fatalf("local HEAD moved to %q, want unchanged %q", head, localHead)
	}
	if !f.fileExists(dest, "LOCAL.md") {
		t.Fatal("divergent local file was clobbered")
	}
	if originBefore != f.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.originMain())
	}
}

func TestGitDetachedHeadAdvancesLocalTrunk(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	f.runGit(dest, "checkout", "-q", "--detach", "HEAD")
	detachedAt := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD"))
	want := f.advanceOrigin("v2")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeAdvanced {
		t.Fatalf("outcome = %q, want advanced", got)
	}
	if localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); localMain != want {
		t.Fatalf("local main = %q, want origin %q", localMain, want)
	}
	if head := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD")); head != detachedAt {
		t.Fatalf("HEAD moved to %q, want unchanged detached %q", head, detachedAt)
	}
}

func TestGitNeverPushOnAdvance(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Local main ahead of origin.
	f.writeFile(dest, "AHEAD.md", "ahead\n")
	f.runGit(dest, "add", "AHEAD.md")
	f.runGit(dest, "commit", "-qm", "local ahead")

	originBefore := f.originMain()
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if originBefore != f.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.originMain())
	}
}

// TestGitPushTrunkFastForward proves a clean fast-forward push: local main is
// strictly ahead of an unmoved origin, so PushTrunk reports pushed and origin
// main advances to the local commit.
func TestGitPushTrunkFastForward(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Local main strictly ahead of origin; origin not moved.
	f.writeFile(dest, "AHEAD.md", "ahead\n")
	f.runGit(dest, "add", "AHEAD.md")
	f.runGit(dest, "commit", "-qm", "local ahead")
	localHead := strings.TrimSpace(f.runGit(dest, "rev-parse", "main"))

	got, err := r.PushTrunk(context.Background())
	if err != nil {
		t.Fatalf("push trunk: %v", err)
	}
	if got != OutcomePushed {
		t.Fatalf("outcome = %q, want pushed", got)
	}
	if origin := f.originMain(); origin != localHead {
		t.Fatalf("origin main = %q, want local head %q", origin, localHead)
	}
}

// TestGitPushTrunkDivergedSkips proves a diverged trunk is not force-pushed: local
// main is ahead AND origin has moved (behind > 0), so PushTrunk skips with
// up-to-date, no error, and origin is left unchanged.
func TestGitPushTrunkDivergedSkips(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Diverge: a local commit on main, then an unrelated commit on origin.
	f.writeFile(dest, "AHEAD.md", "ahead\n")
	f.runGit(dest, "add", "AHEAD.md")
	f.runGit(dest, "commit", "-qm", "local ahead")
	f.advanceOrigin("v2")

	// Advance fetches origin (updating origin/main) and refuses the non-ff merge,
	// leaving local main put; this is the prior fetch PushTrunk relies on.
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}

	originBefore := f.originMain()
	got, err := r.PushTrunk(context.Background())
	if err != nil {
		t.Fatalf("push trunk: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date (diverged, skip)", got)
	}
	if originBefore != f.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.originMain())
	}
}

// TestGitPushTrunkNotAheadNoop proves a not-ahead trunk is a no-op: a fresh clone
// sits exactly at origin, so PushTrunk reports up-to-date and origin is unchanged.
func TestGitPushTrunkNotAheadNoop(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	originBefore := f.originMain()
	got, err := r.PushTrunk(context.Background())
	if err != nil {
		t.Fatalf("push trunk: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date (not ahead)", got)
	}
	if originBefore != f.originMain() {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, f.originMain())
	}
}

// TestGitInUseGeneratedOnlyNotBusy proves a working tree whose only uncommitted
// edit is to a linguist-generated file is not busy from the dirt check. The fresh
// clone's reflog still counts as recent activity under a normal idle window, so a
// tiny idle window isolates the dirt classification as the asserted result.
func TestGitInUseGeneratedOnlyNotBusy(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.writeFile(dest, "build.gen", "local generated edit\n")

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if busy {
		t.Fatalf("InUse = busy (%q), want not busy on generated-only dirt", reason)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty", reason)
	}
}

// TestGitInUseMixedDirtyIsBusy proves a tree dirty in both a generated and a
// non-generated file is busy: the dirt is not generated-only.
func TestGitInUseMixedDirtyIsBusy(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.writeFile(dest, "build.gen", "local generated edit\n")
	f.writeFile(dest, "foo.txt", "real work\n")

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on mixed dirt")
	}
	if reason != "dirty working tree" {
		t.Fatalf("reason = %q, want dirty working tree", reason)
	}
}

// TestGitInUseDirtyGitattributesIsBusy proves an uncommitted edit to .gitattributes
// itself is busy: .gitattributes is not a generated path.
func TestGitInUseDirtyGitattributesIsBusy(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.writeFile(dest, ".gitattributes", "*.gen linguist-generated\n*.foo linguist-generated\n")

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on dirty .gitattributes")
	}
	if reason != "dirty working tree" {
		t.Fatalf("reason = %q, want dirty working tree", reason)
	}
}

// TestGitInUseRenameToGeneratedIsBusy proves a rename of a non-generated file into
// a generated-named path is busy: the rename source is non-generated, so the dirt
// is not generated-only and the uncommitted rename of real work is never discarded.
func TestGitInUseRenameToGeneratedIsBusy(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.runGit(dest, "mv", "README.md", "out.gen")

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on rename of a non-generated file into a generated path")
	}
	if reason != "dirty working tree" {
		t.Fatalf("reason = %q, want dirty working tree", reason)
	}
}

// TestGitAdvanceGeneratedCleanApply proves a generated-only edit that trunk does
// not touch is carried untouched through the fast-forward, reporting rebased-generated.
func TestGitAdvanceGeneratedCleanApply(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	f.writeFile(dest, "build.gen", "local generated edit\n")
	want := f.advanceOriginPath("x.txt", "sibling on trunk\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); localMain != want {
		t.Fatalf("local main = %q, want origin %q", localMain, want)
	}
	if c := f.readFile(dest, "build.gen"); c != "local generated edit\n" {
		t.Fatalf("build.gen = %q, want local edit preserved", c)
	}
}

// TestGitAdvanceGeneratedConflictTakesUpstream proves a generated-only edit that
// conflicts with what trunk changed is discarded in favor of upstream content.
func TestGitAdvanceGeneratedConflictTakesUpstream(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	f.writeFile(dest, "build.gen", "local generated edit\n")
	want := f.advanceOriginPath("build.gen", "trunk generated v2\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); localMain != want {
		t.Fatalf("local main = %q, want origin %q", localMain, want)
	}
	if c := f.readFile(dest, "build.gen"); c != "trunk generated v2\n" {
		t.Fatalf("build.gen = %q, want upstream content (local discarded)", c)
	}
}

// TestGitAdvanceGeneratedBehindZeroNoOp proves a generated-only dirt with trunk
// not moved is a no-op: up-to-date, local edit intact, main unchanged.
func TestGitAdvanceGeneratedBehindZeroNoOp(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	f.writeFile(dest, "build.gen", "local generated edit\n")
	mainBefore := strings.TrimSpace(f.runGit(dest, "rev-parse", "main"))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date", got)
	}
	if c := f.readFile(dest, "build.gen"); c != "local generated edit\n" {
		t.Fatalf("build.gen = %q, want local edit untouched", c)
	}
	if mainAfter := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); mainAfter != mainBefore {
		t.Fatalf("local main moved from %q to %q, want unchanged", mainBefore, mainAfter)
	}
}

// TestGitAdvanceUntrackedGeneratedPreserved proves an untracked generated file that
// trunk does not touch survives a generated-aware advance with its local content.
func TestGitAdvanceUntrackedGeneratedPreserved(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	f.writeFile(dest, "extra.gen", "untracked local\n")

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if busy {
		t.Fatalf("InUse = busy (%q), want not busy on untracked generated file", reason)
	}

	want := f.advanceOriginPath("y.txt", "sibling on trunk\n")
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); localMain != want {
		t.Fatalf("local main = %q, want origin %q", localMain, want)
	}
	if !f.fileExists(dest, "extra.gen") {
		t.Fatal("untracked generated file was clobbered")
	}
	if c := f.readFile(dest, "extra.gen"); c != "untracked local\n" {
		t.Fatalf("extra.gen = %q, want local content preserved", c)
	}
}

// TestGitAdvanceStagedGeneratedAdvances proves a STAGED generated edit does not
// wedge the fast-forward: with a staged edit on a trunk-changed generated path and
// a staged edit on a trunk-untouched generated path, advance resets the index plus
// worktree of the conflict path (taking upstream) and carries the untouched path,
// reporting rebased-generated rather than hard-erroring on `git merge --ff-only`.
func TestGitAdvanceStagedGeneratedAdvances(t *testing.T) {
	f := newFixture(t)
	f.seedGenerated()
	// A second generated file on trunk that the upcoming trunk commit will not touch.
	f.writeFile(f.seed, "keep.gen", "trunk keep v1\n")
	f.runGit(f.seed, "add", "keep.gen")
	f.runGit(f.seed, "commit", "-qm", "seed keep.gen")
	f.runGit(f.seed, "push", "-q", "origin", "main")

	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Stage a generated edit on a trunk-untouched path and on a trunk-changed path.
	f.writeFile(dest, "keep.gen", "local keep edit\n")
	f.writeFile(dest, "build.gen", "local generated edit\n")
	f.runGit(dest, "add", "keep.gen", "build.gen")

	// Trunk changes build.gen (the conflict path) but leaves keep.gen alone.
	want := f.advanceOriginPath("build.gen", "trunk generated v2\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); localMain != want {
		t.Fatalf("local main = %q, want origin %q", localMain, want)
	}
	if c := f.readFile(dest, "build.gen"); c != "trunk generated v2\n" {
		t.Fatalf("build.gen = %q, want upstream content (staged local discarded)", c)
	}
	if c := f.readFile(dest, "keep.gen"); c != "local keep edit\n" {
		t.Fatalf("keep.gen = %q, want staged local content preserved", c)
	}
}

// TestGitInUseUntrackedDirNonGeneratedIsBusy proves an untracked directory holding a
// non-generated file is busy. `git status --porcelain` without -uall collapses the
// directory into one '?? gendir/' record; with a directory-level generated attribute
// that record resolves generated, so the real (non-generated) file inside would be
// wrongly classified as generated-only and skipped. -uall lists the file
// individually, exposing it as a real dirty path so the tree is correctly busy.
func TestGitInUseUntrackedDirNonGeneratedIsBusy(t *testing.T) {
	f := newFixture(t)
	// Mark a whole directory generated, so the collapsed '?? gendir/' record itself
	// resolves linguist-generated even though a non-generated file lives inside.
	f.writeFile(f.seed, ".gitattributes", "*.gen linguist-generated\ngendir/ linguist-generated\n")
	f.runGit(f.seed, "add", ".gitattributes")
	f.runGit(f.seed, "commit", "-qm", "seed dir attr")
	f.runGit(f.seed, "push", "-q", "origin", "main")

	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	if err := os.MkdirAll(filepath.Join(dest, "gendir"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.writeFile(dest, filepath.Join("gendir", "real.txt"), "real work\n")

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on untracked directory with a non-generated file")
	}
	if reason != "dirty working tree" {
		t.Fatalf("reason = %q, want dirty working tree", reason)
	}
}
