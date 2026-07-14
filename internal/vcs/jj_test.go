package vcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

func openJJ(t *testing.T, path string) Repo {
	t.Helper()
	r, err := Open(path, "main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r.Kind() != "jj" {
		t.Fatalf("kind = %q, want jj", r.Kind())
	}
	return r
}

func TestJJHasTrunkAndOrigin(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	ok, err := r.HasTrunk(context.Background())
	if err != nil {
		t.Fatalf("has trunk: %v", err)
	}
	if !ok {
		t.Fatal("HasTrunk = false, want true on colocated clone")
	}
	origin, err := r.Origin(context.Background())
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	if origin != f.Origin {
		t.Fatalf("origin = %q, want %q", origin, f.Origin)
	}
	hash, err := r.TrunkHash(context.Background())
	if err != nil {
		t.Fatalf("trunk hash: %v", err)
	}
	if hash != f.OriginMain() {
		t.Fatalf("trunk hash = %q, want %q", hash, f.OriginMain())
	}
}

func TestJJAdvanceIdleEmpty(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	want := f.AdvanceOrigin("v2")
	oldChange := jjChangeID(t, f, dest)
	snapsBefore := f.JJSnapshotOps(dest)
	opBefore := strings.TrimSpace(f.RunJJ(dest, "op", "log", "-n", "1", "--no-graph", "--ignore-working-copy", "-T", `id ++ "\n"`))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeAdvanced {
		t.Fatalf("outcome = %q, want advanced", got)
	}
	// @ is empty and sits on the new trunk.
	probe := f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy",
		"-T", `"empty=" ++ empty ++ " parent=" ++ parents.map(|c| c.commit_id()).join(",") ++ "\n"`)
	probe = strings.TrimSpace(probe)
	if !strings.HasPrefix(probe, "empty=true ") {
		t.Fatalf("@ not empty after advance: %q", probe)
	}
	if !strings.Contains(probe, "parent="+want) {
		t.Fatalf("@ parent != new trunk %q: %q", want, probe)
	}
	if h, _ := r.TrunkHash(context.Background()); h != want {
		t.Fatalf("trunk hash = %q, want %q", h, want)
	}
	// The clean advance auto-abandons the empty @ (the changeSurvived contract),
	// records exactly one jj new and one fetch op, and never snapshots.
	present := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "present("+oldChange+")", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
	if present != "" {
		t.Fatalf("pre-advance @ %q survived, want auto-abandoned by clean advance", oldChange)
	}
	wantOps := []string{"new empty commit", "fetch from git remote(s) origin"}
	if ops := jjOpsSince(t, f, dest, opBefore); !slices.Equal(ops, wantOps) {
		t.Fatalf("ops since advance = %v, want exactly %v", ops, wantOps)
	}
	if snaps := f.JJSnapshotOps(dest); snaps != snapsBefore {
		t.Fatalf("snapshot ops = %d, want %d (clean advance must not snapshot)", snaps, snapsBefore)
	}
}

// jjChangeID returns @'s full change id.
func jjChangeID(t *testing.T, f *vcstest.Fixture, repo string) string {
	t.Helper()
	return strings.TrimSpace(f.RunJJ(repo, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
}

// jjOpsSince returns the op descriptions recorded after sinceOp, newest first.
func jjOpsSince(t *testing.T, f *vcstest.Fixture, repo, sinceOp string) []string {
	t.Helper()
	out := f.RunJJ(repo, "op", "log", "-n", "20", "--no-graph", "--ignore-working-copy", "-T", `id ++ " " ++ description.first_line() ++ "\n"`)
	var descs []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, desc, _ := strings.Cut(line, " ")
		if id == sinceOp {
			return descs
		}
		descs = append(descs, desc)
	}
	t.Fatalf("op %s not found in the recent op log", sinceOp)
	return nil
}

func TestJJAdvanceUpToDate(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date", got)
	}
}

// TestJJStableDetectsGitHeadDrift proves the guard catches the exact colocated
// footgun: a raw `git commit` moves git HEAD but records NO jj op, so jj's op log
// is blind to it. stable holds on a quiet repo and reports drift once HEAD moves,
// even though the jj op head is unchanged.
func TestJJStableDetectsGitHeadDrift(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	f.ConfigGit(dest) // a raw git commit below needs a git identity in the colocated repo
	r := openJJ(t, dest).(*jjRepo)
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
		t.Fatal("stable = false on a quiet colocated repo, want true")
	}

	opBefore := f.JJOpHead(dest)
	f.WriteFile(dest, "raw.txt", "raw commit\n")
	f.RunGit(dest, "add", "raw.txt")
	f.RunGit(dest, "commit", "-qm", "raw user commit")

	if op := f.JJOpHead(dest); op != opBefore {
		t.Fatalf("jj op head moved %q -> %q: a raw git commit must record no jj op", opBefore, op)
	}
	ok, err = g.stable(ctx)
	if err != nil {
		t.Fatalf("stable (moved): %v", err)
	}
	if ok {
		t.Fatal("stable = true after a raw git commit moved HEAD (op log blind to it), want false")
	}
}

// TestJJStableDetectsDriftAfterProbeSnapshot walks Advance's exact guard sequence
// — guardHead, then the working-copy probe's true snapshot — and proves the guard
// is still live for the mutation that follows: the probe's own snapshot never
// perturbs git HEAD (no false abort), while a raw `git commit` landing after the
// snapshot still reads as drift.
func TestJJStableDetectsDriftAfterProbeSnapshot(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	f.ConfigGit(dest) // a raw git commit below needs a git identity in the colocated repo
	r := openJJ(t, dest).(*jjRepo)
	ctx := context.Background()

	f.WriteFile(dest, "WORK.txt", "live edit\n")
	g, err := r.guardHead(ctx)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	snapshots := f.JJSnapshotOps(dest)
	p, err := r.probeWorkingCopy(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if p.empty || p.described || p.bookmarked {
		t.Fatalf("probe = %+v, want a non-empty undescribed unbookmarked @", p)
	}
	if n := f.JJSnapshotOps(dest); n != snapshots+1 {
		t.Fatalf("snapshot ops %d -> %d: probeWorkingCopy must record the one true snapshot", snapshots, n)
	}
	ok, err := g.stable(ctx)
	if err != nil {
		t.Fatalf("stable: %v", err)
	}
	if !ok {
		t.Fatal("stable = false after the probe's own snapshot, want true (a snapshot must not read as drift)")
	}

	f.WriteFile(dest, "raw.txt", "raw commit\n")
	f.RunGit(dest, "add", "raw.txt")
	f.RunGit(dest, "commit", "-qm", "raw user commit")

	ok, err = g.stable(ctx)
	if err != nil {
		t.Fatalf("stable (drifted): %v", err)
	}
	if ok {
		t.Fatal("stable = true after a raw git commit moved HEAD post-snapshot, want drift")
	}
}

// TestJJAdvanceAbortsUnderLock proves the jj fetch gate: a working_copy.lock at the
// start of Advance yields OutcomeRaced with no fetch and no snapshot, even though
// trunk moved and a clean advance would otherwise run jj new. The op head is
// unchanged, proving the daemon never touched the repo.
func TestJJAdvanceAbortsUnderLock(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")

	opBefore := f.JJOpHead(dest)
	lock := filepath.Join(dest, ".jj", "working_copy", "working_copy.lock")
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
	if op := f.JJOpHead(dest); op != opBefore {
		t.Fatalf("op head moved %q -> %q under a held lock: Advance must not fetch or snapshot", opBefore, op)
	}
}

// TestJJAdvanceRawCommitNotStranded reproduces the end-to-end footgun: a raw git
// commit lands real work with no jj op, then trunk moves and reposync advances.
// The commit must not be orphaned — ancestrySafe sees the imported commit is not on
// trunk and declines, so Advance reports not-disposable and the work stays on disk.
func TestJJAdvanceRawCommitNotStranded(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	f.ConfigGit(dest) // a raw git commit below needs a git identity in the colocated repo
	r := openJJ(t, dest)

	// The footgun: commit real work through raw git, moving HEAD with no jj op.
	f.WriteFile(dest, "work.txt", "hard-won work\n")
	f.RunGit(dest, "add", "work.txt")
	f.RunGit(dest, "commit", "-qm", "raw user commit")

	want := f.AdvanceOrigin("v2")
	originBefore := f.OriginMain()

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable (raw-committed work must not be stranded)", got)
	}
	if !f.FileExists(dest, "work.txt") {
		t.Fatal("work.txt gone: raw-committed work was orphaned")
	}
	if c := f.ReadFile(dest, "work.txt"); c != "hard-won work\n" {
		t.Fatalf("work.txt = %q, want the raw-committed content intact", c)
	}
	if originBefore != f.OriginMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.OriginMain())
	}
	if want == "" {
		t.Fatal("fixture precondition: advanceOrigin returned empty hash")
	}
}

// TestJJLastActivity proves LastActivity returns the most recent non-noise op
// start time. A fresh colocated clone already records real ops (fetch, checkout),
// and a user operation (describe) adds another; both must surface as a recent,
// non-zero time. The zero/no-activity case is unreachable here: jj always records
// the clone's real ops, so the op log is never empty or noise-only.
func TestJJLastActivity(t *testing.T) {
	t.Run("recent after clone", func(t *testing.T) {
		f := vcstest.New(t)
		r := openJJ(t, f.JJClone(filepath.Join(f.Root, "clone")))

		got, err := r.LastActivity(context.Background())
		if err != nil {
			t.Fatalf("last activity: %v", err)
		}
		if got.IsZero() {
			t.Fatal("LastActivity = zero, want a recent clone op time")
		}
		if since := time.Since(got); since > time.Hour {
			t.Fatalf("LastActivity = %v (%v ago), want within the last hour", got, since)
		}
	})

	t.Run("recent after describe operation", func(t *testing.T) {
		f := vcstest.New(t)
		dest := f.JJClone(filepath.Join(f.Root, "clone"))
		r := openJJ(t, dest)
		f.RunJJ(dest, "describe", "-m", "real work", "--ignore-working-copy")

		got, err := r.LastActivity(context.Background())
		if err != nil {
			t.Fatalf("last activity: %v", err)
		}
		if got.IsZero() {
			t.Fatal("LastActivity = zero, want the describe op time")
		}
		if since := time.Since(got); since > time.Hour {
			t.Fatalf("LastActivity = %v (%v ago), want within the last hour", got, since)
		}
	})
}

// TestJJLastActivityRealOpBuriedUnderNoise proves LastActivity pages past
// reposync's own noise: a real user op buried under more than one op-log page of
// snapshot ops (the shape of a peer-notify fetch storm) is still found, not
// misread as no activity.
func TestJJLastActivityRealOpBuriedUnderNoise(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.RunJJ(dest, "describe", "-m", "real work", "--ignore-working-copy")
	// Distinct sizes so every write is seen as dirty and records its own snapshot op.
	for i := range opLogPage + 5 {
		f.WriteFile(dest, "noise.txt", strings.Repeat("n", i+1)+"\n")
		f.SnapshotJJ(dest)
	}
	if n := f.JJSnapshotOps(dest); n <= opLogPage {
		t.Fatalf("fixture precondition: %d snapshot ops, want > %d to bury the real op past one page", n, opLogPage)
	}

	got, err := r.LastActivity(context.Background())
	if err != nil {
		t.Fatalf("last activity: %v", err)
	}
	if got.IsZero() {
		t.Fatal("LastActivity = zero: the real op was lost under the noise burial")
	}
	if since := time.Since(got); since > time.Hour {
		t.Fatalf("LastActivity = %v (%v ago), want within the last hour", got, since)
	}
}

func TestJJInUseDirtyNoClobber(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// A real edit plus the snapshot a normal jj command takes; the poller never
	// snapshots, so the file must be recorded by genuine activity to be seen.
	f.WriteFile(dest, "WORK.txt", "in progress\n")
	f.SnapshotJJ(dest)

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on dirty @")
	}
	if reason != "dirty working copy" {
		t.Fatalf("reason = %q, want dirty working copy", reason)
	}

	// Move trunk so Advance reaches the disposability check rather than
	// short-circuiting on an unmoved trunk.
	f.AdvanceOrigin("v2")
	opsBefore := f.RunJJ(dest, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `id.short() ++ "\n"`)
	// Advance must return not-disposable and leave the change and op log intact.
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable", got)
	}
	if !f.FileExists(dest, "WORK.txt") {
		t.Fatal("in-progress file was clobbered")
	}
	if got := f.ReadFile(dest, "WORK.txt"); got != "in progress\n" {
		t.Fatalf("in-progress file content changed to %q", got)
	}
	opsAfter := f.RunJJ(dest, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `id.short() ++ "\n"`)
	if !strings.Contains(opsAfter, strings.SplitN(strings.TrimSpace(opsBefore), "\n", 2)[0]) {
		t.Fatal("op log head changed: op history was disturbed")
	}
}

// TestJJInUseUnsnapshottedDirty edits a tracked file on disk with NO intervening
// jj command. InUse never snapshots, so the edit is invisible to its dirty probe:
// within the idle window the recency gate covers it (a real edit follows real jj
// ops in practice), and past the window InUse reads not-busy without appending an
// op. The authoritative no-clobber guard for this state is Advance's true
// snapshot, proven by TestJJUnsnapshottedNoClobber.
func TestJJInUseUnsnapshottedDirty(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// Modify a tracked file directly on disk; run no jj command before InUse.
	f.WriteFile(dest, "README.md", "hello\nedited but not snapshotted\n")

	// Within the idle window the clone's recent real ops read as activity.
	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on recent clone ops")
	}
	if !strings.HasPrefix(reason, "recent activity: ") {
		t.Fatalf("reason = %q, want recent activity prefix", reason)
	}

	// Past the idle window the probe reads the last-recorded @ without
	// snapshotting: not busy, and no op is appended.
	head := f.JJOpHead(dest)
	busy, reason, err = r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if busy {
		t.Fatalf("InUse = busy (%q), want not busy: the probe must not snapshot the on-disk edit", reason)
	}
	if got := f.JJOpHead(dest); got != head {
		t.Fatalf("op head moved %q -> %q: InUse snapshotted the working copy", head, got)
	}
}

// TestJJUnsnapshottedNoClobber proves the no-clobber guarantee over the
// unsnapshotted window under snapshot-free InUse: a tracked file is edited on
// disk (no jj command), origin advances, and the op log reads idle. Advance's
// true disposability snapshot — the authoritative guard — records the edit and
// declines, leaving it intact on disk and retained in @.
func TestJJUnsnapshottedNoClobber(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	const edited = "hello\nlive edit\n"
	f.WriteFile(dest, "README.md", edited)
	f.AdvanceOrigin("v2")

	// Snapshot-free InUse cannot see the unsnapshotted edit once the ops are idle.
	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if busy {
		t.Fatalf("InUse = busy (%q), want not busy so Advance is the guard under test", reason)
	}

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable (true snapshot must see the edit)", got)
	}

	// The edit is intact on disk and retained in @.
	if got := f.ReadFile(dest, "README.md"); got != edited {
		t.Fatalf("README.md on disk = %q, want %q", got, edited)
	}
	diff := f.RunJJ(dest, "diff", "-r", "@", "--git")
	if !strings.Contains(diff, "live edit") {
		t.Fatalf("jj diff does not retain the live edit:\n%s", diff)
	}
}

// TestJJInUseSnapshotFree proves InUse appends no operation on any probe path:
// clean, recorded-dirty, and recorded generated-only working copies.
func TestJJInUseSnapshotFree(t *testing.T) {
	cases := []struct {
		id         string
		generated  bool
		setup      func(f *vcstest.Fixture, dest string)
		wantBusy   bool
		wantReason string
	}{
		{"clean", false, func(*vcstest.Fixture, string) {}, false, ""},
		{"recorded dirty", false, func(f *vcstest.Fixture, dest string) {
			f.WriteFile(dest, "WORK.txt", "in progress\n")
			f.SnapshotJJ(dest)
		}, true, "dirty working copy"},
		{"generated only", true, func(f *vcstest.Fixture, dest string) {
			f.WriteFile(dest, "build.gen", "local generated edit\n")
			f.SnapshotJJ(dest)
		}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			f := vcstest.New(t)
			if c.generated {
				f.SeedGenerated()
			}
			dest := f.JJClone(filepath.Join(f.Root, "clone"))
			r := openJJ(t, dest)
			c.setup(f, dest)

			head := f.JJOpHead(dest)
			busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
			if err != nil {
				t.Fatalf("in use: %v", err)
			}
			if busy != c.wantBusy || reason != c.wantReason {
				t.Fatalf("InUse = (%v, %q), want (%v, %q)", busy, reason, c.wantBusy, c.wantReason)
			}
			if got := f.JJOpHead(dest); got != head {
				t.Fatalf("op head moved %q -> %q: InUse snapshotted the working copy", head, got)
			}
		})
	}
}

// TestJJInUseRecencyGateFirst proves a fresh real op reads as busy before any
// working-copy probe runs: no op is appended even with a live on-disk edit that
// a snapshotting probe would have recorded.
func TestJJInUseRecencyGateFirst(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.RunJJ(dest, "describe", "-m", "real work", "--ignore-working-copy")
	f.WriteFile(dest, "WORK.txt", "live edit\n")

	head := f.JJOpHead(dest)
	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on a fresh real op")
	}
	if !strings.HasPrefix(reason, "recent activity: ") {
		t.Fatalf("reason = %q, want recent activity prefix", reason)
	}
	if got := f.JJOpHead(dest); got != head {
		t.Fatalf("op head moved %q -> %q: InUse snapshotted the working copy", head, got)
	}
}

// TestJJAdvanceTrunkNotMovedSnapshotFree proves the steady-state Advance path
// never snapshots: an unmoved trunk short-circuits to up-to-date before the
// disposability probe, leaving a live on-disk edit unrecorded and intact.
func TestJJAdvanceTrunkNotMovedSnapshotFree(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	const edited = "hello\nlive edit\n"
	f.WriteFile(dest, "README.md", edited)

	snapshots := f.JJSnapshotOps(dest)
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date", got)
	}
	if n := f.JJSnapshotOps(dest); n != snapshots {
		t.Fatalf("snapshot ops %d -> %d: Advance snapshotted with trunk unmoved", snapshots, n)
	}
	if got := f.ReadFile(dest, "README.md"); got != edited {
		t.Fatalf("README.md on disk = %q, want %q", got, edited)
	}
}

// TestJJAdvanceUpdatesWorkingCopy proves jj new advances the working copy on disk:
// a tracked file added on new main must appear with new-main content after Advance,
// not leave a stale working copy at old main.
func TestJJAdvanceUpdatesWorkingCopy(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// Origin gains a new tracked file that does not exist in the clone's working copy.
	f.WriteFile(f.Seed, "NEW.txt", "from new main\n")
	f.RunGit(f.Seed, "add", "NEW.txt")
	f.RunGit(f.Seed, "commit", "-qm", "add NEW.txt")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	if f.FileExists(dest, "NEW.txt") {
		t.Fatal("NEW.txt present before advance: fixture precondition broken")
	}

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeAdvanced {
		t.Fatalf("outcome = %q, want advanced", got)
	}
	if !f.FileExists(dest, "NEW.txt") {
		t.Fatal("NEW.txt absent after advance: working copy left stale")
	}
	if c := f.ReadFile(dest, "NEW.txt"); c != "from new main\n" {
		t.Fatalf("NEW.txt content = %q, want new-main content", c)
	}
}

func TestJJAdvanceNotDisposableWithDescription(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")

	changeBefore := f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`)
	f.RunJJ(dest, "describe", "-m", "work in progress", "--ignore-working-copy")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable", got)
	}
	changeAfter := f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`)
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	desc := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `description.first_line()`))
	if desc != "work in progress" {
		t.Fatalf("description = %q, want preserved", desc)
	}
}

func TestJJNoOrigin(t *testing.T) {
	f := vcstest.New(t)
	dest := filepath.Join(f.Root, "noorigin")
	f.JJInit(dest)
	r := openJJ(t, dest)

	_, err := r.Origin(context.Background())
	if !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("origin err = %v, want ErrNoOrigin", err)
	}
	ok, err := r.HasTrunk(context.Background())
	if err != nil {
		t.Fatalf("has trunk: %v", err)
	}
	if ok {
		t.Fatal("HasTrunk = true, want false on remoteless repo")
	}
}

func TestJJNeverPushOnAdvance(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")

	originBefore := f.OriginMain()
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if originBefore != f.OriginMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.OriginMain())
	}
}

// TestJJPushTrunkFastForward proves a clean fast-forward push on a colocated jj
// clone: the local main bookmark is moved ahead onto a real described commit with
// origin unmoved, so PushTrunk reports pushed and origin main advances to it.
func TestJJPushTrunkFastForward(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// Real local content on @, then point the main bookmark at it: local main is
	// now strictly ahead of main@origin (origin not moved).
	f.WriteFile(dest, "AHEAD.md", "ahead\n")
	f.RunJJ(dest, "describe", "-m", "local ahead", "--ignore-working-copy")
	f.RunJJ(dest, "bookmark", "set", "main", "-r", "@", "--ignore-working-copy")
	localMain := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "main", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))

	got, err := r.PushTrunk(context.Background())
	if err != nil {
		t.Fatalf("push trunk: %v", err)
	}
	if got != OutcomePushed {
		t.Fatalf("outcome = %q, want pushed", got)
	}
	if origin := f.OriginMain(); origin != localMain {
		t.Fatalf("origin main = %q, want local main bookmark %q", origin, localMain)
	}
}

// TestJJPushTrunkDivergedSkips proves a diverged trunk is not force-pushed: the
// local main bookmark is moved ahead AND origin moves independently, so after the
// fetch the local main bookmark is conflicted. Advance classifies such a
// divergence (diverged, no error) like the git backend, and PushTrunk skips
// quietly on the conflicted-bookmark revset error: up-to-date, no error, origin
// unchanged.
func TestJJPushTrunkDivergedSkips(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// Diverge: move local main onto real content, then advance origin separately.
	f.WriteFile(dest, "AHEAD.md", "ahead\n")
	f.RunJJ(dest, "describe", "-m", "local ahead", "--ignore-working-copy")
	f.RunJJ(dest, "bookmark", "set", "main", "-r", "@", "--ignore-working-copy")
	f.AdvanceOrigin("v2")

	// Advance performs the fetch PushTrunk relies on; the fetch leaves the local
	// main bookmark conflicted. Advance must classify the divergence and decline
	// untouched, matching git's structural classification rather than surfacing
	// the conflict as an error.
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: want diverged decline on conflicted bookmark, got error: %v", err)
	}
	if got != OutcomeDiverged {
		t.Fatalf("advance outcome = %q, want diverged (declined untouched)", got)
	}
	conflicted := strings.TrimSpace(f.RunJJ(dest, "bookmark", "list", "main", "--ignore-working-copy",
		"-T", `name ++ " conflict=" ++ conflict ++ "\n"`))
	if !strings.Contains(conflicted, "main conflict=true") {
		t.Fatalf("main bookmark not conflicted after diverged fetch:\n%s", conflicted)
	}

	originBefore := f.OriginMain()
	got, err = r.PushTrunk(context.Background())
	if err != nil {
		t.Fatalf("push trunk: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date (diverged, skip)", got)
	}
	if originBefore != f.OriginMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.OriginMain())
	}
}

// jjGeneratedOnlyProbe renders @'s emptiness, description, and bookmarks with the
// production probe template so a test can assert @ still carries only a generated
// edit ("f f f": non-empty, no description, no bookmarks).
func jjGeneratedOnlyProbe(t *testing.T, f *vcstest.Fixture, repo string) string {
	t.Helper()
	out := f.RunJJ(repo, "log", "-r", "@", "--no-graph", "--ignore-working-copy",
		"-T", `separate(" ", if(empty, "t", "f"), if(description, "t", "f"), if(bookmarks, "t", "f")) ++ "\n"`)
	return strings.TrimSpace(out)
}

// jjParent returns @'s parent commit id.
func jjParent(t *testing.T, f *vcstest.Fixture, repo string) string {
	t.Helper()
	out := f.RunJJ(repo, "log", "-r", "@", "--no-graph", "--ignore-working-copy",
		"-T", `parents.map(|c| c.commit_id()).join(",") ++ "\n"`)
	return strings.TrimSpace(out)
}

// TestJJInUseGeneratedOnlyNotBusy proves a @ whose only change is a generated file
// is not busy from the dirt check.
func TestJJInUseGeneratedOnlyNotBusy(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if busy {
		t.Fatalf("InUse = busy (%q), want not busy on generated-only @", reason)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty", reason)
	}
}

// TestJJInUseMixedDirtyIsBusy proves a @ changing both a generated and a
// non-generated file is busy: the dirt is not generated-only.
func TestJJInUseMixedDirtyIsBusy(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.WriteFile(dest, "WORK.txt", "real work\n")
	f.SnapshotJJ(dest)

	busy, reason, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on mixed dirt")
	}
	if reason != "dirty working copy" {
		t.Fatalf("reason = %q, want dirty working copy", reason)
	}
}

// TestJJAdvanceGeneratedCleanApply proves a generated-only @ is rebased onto a moved
// trunk that does not touch the generated path, keeping the local edit untouched.
func TestJJAdvanceGeneratedCleanApply(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)
	want := f.AdvanceOriginPath("x.txt", "sibling on trunk\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if parent := jjParent(t, f, dest); parent != want {
		t.Fatalf("@ parent = %q, want new trunk %q", parent, want)
	}
	if c := f.ReadFile(dest, "build.gen"); c != "local generated edit\n" {
		t.Fatalf("build.gen = %q, want local edit preserved", c)
	}
	if probe := jjGeneratedOnlyProbe(t, f, dest); probe != "f f f" {
		t.Fatalf("@ probe = %q, want f f f (non-empty generated-only @)", probe)
	}
}

// TestJJAdvanceGeneratedConflictTakesUpstream proves a generated-only @ that
// conflicts with what trunk changed is resolved by restoring trunk's content.
func TestJJAdvanceGeneratedConflictTakesUpstream(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)
	want := f.AdvanceOriginPath("build.gen", "trunk generated v2\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if parent := jjParent(t, f, dest); parent != want {
		t.Fatalf("@ parent = %q, want new trunk %q", parent, want)
	}
	conflicts := strings.TrimSpace(f.RunJJConflicts(dest))
	if conflicts != "" {
		t.Fatalf("jj resolve --list = %q, want empty (conflicts resolved)", conflicts)
	}
	if c := f.ReadFile(dest, "build.gen"); c != "trunk generated v2\n" {
		t.Fatalf("build.gen = %q, want upstream content (local discarded)", c)
	}
}

// TestJJAdvanceGeneratedTrunkNotMoved exercises a generated-only @ with trunk not
// moved: nothing to advance onto, so @ must be left untouched. The outcome matches
// the git sibling's behind==0 path, OutcomeUpToDate, for the identical state.
func TestJJAdvanceGeneratedTrunkNotMoved(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)
	changeBefore := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date", got)
	}
	changeAfter := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	if c := f.ReadFile(dest, "build.gen"); c != "local generated edit\n" {
		t.Fatalf("build.gen = %q, want local edit untouched", c)
	}
}

// TestJJAdvanceGeneratedAtopUnpushedCommitNotDisposable proves the ancestry safety
// gate: a generated-only @ sitting on top of an UNPUSHED described commit must not be
// rebased onto trunk, since that would strand the local commit and strip its files
// from the working copy. Advance returns not-disposable, the described commit stays an
// ancestor of @, and the local source file is intact on disk.
func TestJJAdvanceGeneratedAtopUnpushedCommitNotDisposable(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// An unpushed local commit carrying real work, then describe it. Snapshot the
	// file into @ before describing so it lands in the commit, not in a later @.
	f.WriteFile(dest, "realsource.txt", "in progress real work\n")
	f.SnapshotJJ(dest)
	f.RunJJ(dest, "describe", "-m", "local unpushed work", "--ignore-working-copy")
	unpushed := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))

	// A generated-only @ on top of that unpushed commit.
	f.RunJJ(dest, "new", "--ignore-working-copy")
	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)

	// Move trunk so Advance reaches the disposability / ancestry checks.
	f.AdvanceOriginPath("x.txt", "sibling on trunk\n")
	changeBefore := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable (unpushed parent is unsafe to rebase)", got)
	}
	changeAfter := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	// The unpushed described commit is still an ancestor of @.
	ancestors := strings.TrimSpace(f.RunJJ(dest, "log", "-r", unpushed+" & ::@", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))
	if ancestors != unpushed {
		t.Fatalf("unpushed commit %q no longer an ancestor of @ (got %q): work stranded", unpushed, ancestors)
	}
	if c := f.ReadFile(dest, "realsource.txt"); c != "in progress real work\n" {
		t.Fatalf("realsource.txt = %q, want local work intact (not clobbered)", c)
	}
}

// TestJJAdvanceEmptyAtopUnpushedCommitNotDisposable proves the ancestry safety gate
// for an EMPTY working copy: an empty, undescribed, unbookmarked @ sitting on top of
// an UNPUSHED described commit must not be advanced onto a moved trunk. `jj new
// <trunk>` would reparent the working copy onto trunk and strand the local commit,
// reverting the working copy to trunk content. Advance returns not-disposable, @
// stays put, the unpushed commit remains an ancestor of @, and its file is intact.
// This is the empty-@ sibling of TestJJAdvanceGeneratedAtopUnpushedCommitNotDisposable.
func TestJJAdvanceEmptyAtopUnpushedCommitNotDisposable(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	// An unpushed local commit carrying real work; describe it, then create an empty
	// @ on top — the common mid-branch state (committed work + fresh empty @).
	f.WriteFile(dest, "realsource.txt", "in progress real work\n")
	f.SnapshotJJ(dest)
	f.RunJJ(dest, "describe", "-m", "local unpushed work", "--ignore-working-copy")
	unpushed := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))
	f.RunJJ(dest, "new", "--ignore-working-copy")

	// Move trunk so Advance reaches the disposability / ancestry checks.
	f.AdvanceOrigin("v2")
	changeBefore := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable (unpushed parent is unsafe to advance)", got)
	}
	changeAfter := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	// The unpushed described commit is still an ancestor of @.
	ancestors := strings.TrimSpace(f.RunJJ(dest, "log", "-r", unpushed+" & ::@", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))
	if ancestors != unpushed {
		t.Fatalf("unpushed commit %q no longer an ancestor of @ (got %q): work stranded", unpushed, ancestors)
	}
	if c := f.ReadFile(dest, "realsource.txt"); c != "in progress real work\n" {
		t.Fatalf("realsource.txt = %q, want local work intact (not clobbered)", c)
	}
}

// TestJJAdvanceEmptyAtopFeatureBookmarkNotDisposable proves an empty @ whose parent
// carries a non-trunk bookmark (a named feature branch) is not advanced: jj new
// <trunk> would yank the working copy off the feature line. The bookmarked commit is
// unpushed, so ancestrySafe is false and Advance declines, leaving @ on the feature.
func TestJJAdvanceEmptyAtopFeatureBookmarkNotDisposable(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "feature.txt", "feature work\n")
	f.SnapshotJJ(dest)
	f.RunJJ(dest, "bookmark", "create", "feature", "-r", "@", "--ignore-working-copy")
	feature := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))
	f.RunJJ(dest, "new", "--ignore-working-copy")

	f.AdvanceOrigin("v2")
	changeBefore := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable (empty @ atop a feature bookmark)", got)
	}
	changeAfter := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	onFeature := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "feature & ::@", "--no-graph", "--ignore-working-copy", "-T", `commit_id`))
	if onFeature != feature {
		t.Fatalf("feature commit %q no longer an ancestor of @ (got %q): yanked off branch", feature, onFeature)
	}
	if c := f.ReadFile(dest, "feature.txt"); c != "feature work\n" {
		t.Fatalf("feature.txt = %q, want feature work intact", c)
	}
}

// TestJJAdvanceGeneratedConflictSpacedPath proves a generated path containing a SPACE
// that conflicts with trunk is fully resolved: the `jj resolve --list` parser must
// recover the whole path (not truncate at the first space), so after Advance the
// conflict is cleared and the file holds upstream content.
func TestJJAdvanceGeneratedConflictSpacedPath(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	// A generated file whose name contains a space, seeded on trunk.
	const spaced = "out put.gen"
	f.WriteFile(f.Seed, spaced, "trunk spaced v1\n")
	f.RunGit(f.Seed, "add", spaced)
	f.RunGit(f.Seed, "commit", "-qm", "seed spaced gen")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")

	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, spaced, "local spaced edit\n")
	f.SnapshotJJ(dest)
	want := f.AdvanceOriginPath(spaced, "trunk spaced v2\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRebasedGenerated {
		t.Fatalf("outcome = %q, want rebased-generated", got)
	}
	if parent := jjParent(t, f, dest); parent != want {
		t.Fatalf("@ parent = %q, want new trunk %q", parent, want)
	}
	conflicts := strings.TrimSpace(f.RunJJConflicts(dest))
	if conflicts != "" {
		t.Fatalf("jj resolve --list = %q, want empty (spaced-path conflict resolved)", conflicts)
	}
	if c := f.ReadFile(dest, spaced); c != "trunk spaced v2\n" {
		t.Fatalf("%s = %q, want upstream content (local discarded)", spaced, c)
	}
}

// TestJJAdvanceDescribedGeneratedNotDisposable proves a @ that carries generated
// dirt but also a description is not disposable: real work guards the no-clobber boundary.
func TestJJAdvanceDescribedGeneratedNotDisposable(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)
	f.RunJJ(dest, "describe", "-m", "work in progress", "--ignore-working-copy")
	// Move trunk so Advance reaches the disposability check rather than short-circuiting.
	f.AdvanceOriginPath("x.txt", "sibling on trunk\n")
	changeBefore := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable", got)
	}
	changeAfter := strings.TrimSpace(f.RunJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`))
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	if c := f.ReadFile(dest, "build.gen"); c != "local generated edit\n" {
		t.Fatalf("build.gen = %q, want local edit untouched", c)
	}
}

// TestJJAdvanceSweptMidNewRecovers proves the branch-A TOCTOU recovery: an edit
// typed between Advance's empty-@ classification and its jj new is snapshotted
// into the outgoing commit and swept off disk; Advance detects the survival,
// rebases the swept commit onto the new trunk, and re-materializes it as @.
func TestJJAdvanceSweptMidNewRecovers(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	want := f.AdvanceOrigin("v2")
	oldChange := jjChangeID(t, f, dest)
	f.ShimJJDirtOn("new", dest, "interim.txt", "typed mid-advance\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRecovered {
		t.Fatalf("outcome = %q, want recovered", got)
	}
	if c := f.ReadFile(dest, "interim.txt"); c != "typed mid-advance\n" {
		t.Fatalf("interim.txt = %q, want mid-advance edit restored byte-identical", c)
	}
	if cur := jjChangeID(t, f, dest); cur != oldChange {
		t.Fatalf("@ change id = %q, want original %q", cur, oldChange)
	}
	if parent := jjParent(t, f, dest); parent != want {
		t.Fatalf("@ parent = %q, want new trunk %q (forward recovery)", parent, want)
	}
	heads := strings.Fields(f.RunJJ(dest, "log", "-r", "heads(all())", "--no-graph", "--ignore-working-copy", "-T", `change_id ++ "\n"`))
	if len(heads) != 1 || heads[0] != oldChange {
		t.Fatalf("heads(all()) = %v, want only @ %q (no debris)", heads, oldChange)
	}
}

// TestJJAdvanceGeneratedSweptForeignPathRecovers proves the branch-B TOCTOU
// recovery: prose typed between the generated-only classification and the
// rebase is snapshotted into @ and rebased along with it; the surplus path
// fails verification, and with only reposync's own ops in the window the
// conflicted rebase is undone onto the original parents, where the conflict
// cancels — the trunk-takes-conflicts restores must never run.
func TestJJAdvanceGeneratedSweptForeignPathRecovers(t *testing.T) {
	f := vcstest.New(t)
	f.SeedGenerated()
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)

	f.WriteFile(dest, "build.gen", "local generated edit\n")
	f.SnapshotJJ(dest)
	f.AdvanceOriginPath("build.gen", "trunk generated v2\n")
	oldChange := jjChangeID(t, f, dest)
	oldParent := jjParent(t, f, dest)
	f.ShimJJDirtOn("rebase", dest, "notes.txt", "user prose\n")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeRecovered {
		t.Fatalf("outcome = %q, want recovered", got)
	}
	if c := f.ReadFile(dest, "notes.txt"); c != "user prose\n" {
		t.Fatalf("notes.txt = %q, want mid-advance prose byte-identical", c)
	}
	if c := f.ReadFile(dest, "build.gen"); c != "local generated edit\n" {
		t.Fatalf("build.gen = %q, want LOCAL edit (restores must not run)", c)
	}
	if cur := jjChangeID(t, f, dest); cur != oldChange {
		t.Fatalf("@ change id = %q, want original %q", cur, oldChange)
	}
	if parent := jjParent(t, f, dest); parent != oldParent {
		t.Fatalf("@ parent = %q, want original %q (back-rebased)", parent, oldParent)
	}
	if conflicts := strings.TrimSpace(f.RunJJConflicts(dest)); conflicts != "" {
		t.Fatalf("resolve --list = %q, want no conflicts after back-rebase", conflicts)
	}
}

// TestJJAdvanceSweptOpHeadMovedHandsOff proves the honest failure mode: a user
// op (a bookmark create) lands between jj new and the verification read, so the
// window is foreign and Advance returns swept with zero recovery commands — the
// swept edit stays off disk but preserved in a visible commit.
func TestJJAdvanceSweptOpHeadMovedHandsOff(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")
	oldChange := jjChangeID(t, f, dest)

	shimDir := filepath.Join(f.Root, "shim")
	dirted := filepath.Join(shimDir, "dirted")
	raced := filepath.Join(shimDir, "raced")
	f.ShimJJ(strings.Join([]string{
		`for a in "$@"; do`,
		`  if [ "$a" = new ] && mkdir "` + dirted + `" 2>/dev/null; then`,
		`    printf '%s\n' 'typed mid-advance' > "` + filepath.Join(dest, "interim.txt") + `"`,
		`  fi`,
		`  if [ "$a" = log ] && [ -d "` + dirted + `" ] && mkdir "` + raced + `" 2>/dev/null; then`,
		`    "$REAL_JJ" --repository "` + dest + `" bookmark create raced-marker -r main`,
		`  fi`,
		`done`,
	}, "\n"))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeSwept {
		t.Fatalf("outcome = %q, want swept", got)
	}
	if f.FileExists(dest, "interim.txt") {
		t.Fatal("interim.txt on disk, want swept (hands-off mode restores nothing)")
	}
	swept := f.RunJJ(dest, "diff", "-r", oldChange, "--name-only", "--ignore-working-copy")
	if !strings.Contains(swept, "interim.txt") {
		t.Fatalf("diff -r %s = %q, want interim.txt preserved in the swept commit", oldChange, swept)
	}
	if cur := jjChangeID(t, f, dest); cur == oldChange {
		t.Fatalf("@ change id still %q, want moved by jj new", cur)
	}
	top := strings.TrimSpace(f.RunJJ(dest, "op", "log", "-n", "1", "--no-graph", "--ignore-working-copy", "-T", `description.first_line() ++ "\n"`))
	if !strings.HasPrefix(top, "create bookmark") {
		t.Fatalf("top op = %q, want the raced bookmark create (no recovery commands ran)", top)
	}
}

// TestJJAdvanceSecondSweepDuringRecoverySurfaces proves the second-sweep guard:
// dirt typed just before recovery's jj edit is snapshotted into the interim
// trunk commit, which therefore survives the edit; Advance reports swept even
// though recovery materially completed — the first sweep is back on disk, the
// second sits preserved in the visible interim head.
func TestJJAdvanceSecondSweepDuringRecoverySurfaces(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")
	oldChange := jjChangeID(t, f, dest)

	shimDir := filepath.Join(f.Root, "shim")
	f.ShimJJ(strings.Join([]string{
		`for a in "$@"; do`,
		`  if [ "$a" = new ] && mkdir "` + filepath.Join(shimDir, "dirt1") + `" 2>/dev/null; then`,
		`    printf '%s\n' 'typed mid-advance' > "` + filepath.Join(dest, "interim.txt") + `"`,
		`  fi`,
		`  if [ "$a" = edit ] && mkdir "` + filepath.Join(shimDir, "dirt2") + `" 2>/dev/null; then`,
		`    printf '%s\n' 'typed during recovery' > "` + filepath.Join(dest, "interim2.txt") + `"`,
		`  fi`,
		`done`,
	}, "\n"))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeSwept {
		t.Fatalf("outcome = %q, want swept on second sweep", got)
	}
	if c := f.ReadFile(dest, "interim.txt"); c != "typed mid-advance\n" {
		t.Fatalf("interim.txt = %q, want first sweep restored (recovery materially completed)", c)
	}
	if cur := jjChangeID(t, f, dest); cur != oldChange {
		t.Fatalf("@ change id = %q, want original %q", cur, oldChange)
	}
	if f.FileExists(dest, "interim2.txt") {
		t.Fatal("interim2.txt on disk, want second sweep off-disk")
	}
	heads := strings.Fields(f.RunJJ(dest, "log", "-r", "heads(all())", "--no-graph", "--ignore-working-copy", "-T", `change_id ++ "\n"`))
	preserved := false
	for _, h := range heads {
		if strings.Contains(f.RunJJ(dest, "diff", "-r", h, "--name-only", "--ignore-working-copy"), "interim2.txt") {
			preserved = true
		}
	}
	if !preserved {
		t.Fatalf("no visible head carries interim2.txt; heads = %v", heads)
	}
}

// TestJJAdvanceRecoveryContentionSwept proves working-copy contention during a
// recovery mutation degrades to swept, not an error: the survivor stays a
// visible head and the repo is left for the user.
func TestJJAdvanceRecoveryContentionSwept(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")
	oldChange := jjChangeID(t, f, dest)

	shimDir := filepath.Join(f.Root, "shim")
	f.ShimJJ(strings.Join([]string{
		`for a in "$@"; do`,
		`  if [ "$a" = new ] && mkdir "` + filepath.Join(shimDir, "dirt1") + `" 2>/dev/null; then`,
		`    printf '%s\n' 'typed mid-advance' > "` + filepath.Join(dest, "interim.txt") + `"`,
		`  fi`,
		`  if [ "$a" = edit ] && mkdir "` + filepath.Join(shimDir, "contend") + `" 2>/dev/null; then`,
		`    echo 'Error: Concurrent checkout' >&2`,
		`    exit 1`,
		`  fi`,
		`done`,
	}, "\n"))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeSwept {
		t.Fatalf("outcome = %q, want swept on recovery contention", got)
	}
	if f.FileExists(dest, "interim.txt") {
		t.Fatal("interim.txt on disk, want recovery aborted before the edit materialized it")
	}
	swept := f.RunJJ(dest, "diff", "-r", oldChange, "--name-only", "--ignore-working-copy")
	if !strings.Contains(swept, "interim.txt") {
		t.Fatalf("diff -r %s = %q, want interim.txt still visible in the survivor", oldChange, swept)
	}
}

// TestJJAdvanceSweptConcurrentNewHandsOff pins the op-shape cardinality: a
// user's own jj new lands between the mutation and the verification read,
// emitting an allowlisted-STRING "new empty commit" op that breaks the shape,
// so Advance keeps hands off.
func TestJJAdvanceSweptConcurrentNewHandsOff(t *testing.T) {
	f := vcstest.New(t)
	dest := f.JJClone(filepath.Join(f.Root, "clone"))
	r := openJJ(t, dest)
	f.AdvanceOrigin("v2")
	oldChange := jjChangeID(t, f, dest)

	shimDir := filepath.Join(f.Root, "shim")
	dirted := filepath.Join(shimDir, "dirted")
	f.ShimJJ(strings.Join([]string{
		`for a in "$@"; do`,
		`  if [ "$a" = new ] && mkdir "` + dirted + `" 2>/dev/null; then`,
		`    printf '%s\n' 'typed mid-advance' > "` + filepath.Join(dest, "interim.txt") + `"`,
		`  fi`,
		`  if [ "$a" = log ] && [ -d "` + dirted + `" ] && mkdir "` + filepath.Join(shimDir, "raced") + `" 2>/dev/null; then`,
		`    "$REAL_JJ" --repository "` + dest + `" new`,
		`  fi`,
		`done`,
	}, "\n"))

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeSwept {
		t.Fatalf("outcome = %q, want swept on shape-breaking concurrent jj new", got)
	}
	if f.FileExists(dest, "interim.txt") {
		t.Fatal("interim.txt on disk, want swept (hands-off mode restores nothing)")
	}
	swept := f.RunJJ(dest, "diff", "-r", oldChange, "--name-only", "--ignore-working-copy")
	if !strings.Contains(swept, "interim.txt") {
		t.Fatalf("diff -r %s = %q, want interim.txt preserved in the swept commit", oldChange, swept)
	}
	if cur := jjChangeID(t, f, dest); cur == oldChange {
		t.Fatalf("@ change id still %q, want moved by the raced jj new", cur)
	}
	top := strings.TrimSpace(f.RunJJ(dest, "op", "log", "-n", "1", "--no-graph", "--ignore-working-copy", "-T", `description.first_line() ++ "\n"`))
	if top != "new empty commit" {
		t.Fatalf("top op = %q, want the user's new empty commit (no recovery commands ran)", top)
	}
}
