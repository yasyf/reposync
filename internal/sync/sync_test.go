package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// harness is the shared vcs fixture plus a default_location into which tracked
// repos are cloned; newHarness also redirects state config under the fixture root.
type harness struct {
	*vcstest.Fixture
	t       *testing.T
	dataLoc string // default_location where tracked repos live
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.Root, "xdg"))
	dataLoc := filepath.Join(f.Root, "data")
	if err := os.MkdirAll(dataLoc, 0o750); err != nil {
		t.Fatalf("mkdir data loc: %v", err)
	}
	return &harness{Fixture: f, t: t, dataLoc: dataLoc}
}

// state builds a *state.State pointed at this harness's default_location with a
// short idle threshold so freshly-cloned repos are not seen as recently active.
// PushAfter is likewise 1ns so the push gate treats a quiet repo as quiet; tests
// that need to suppress the push override st.Settings.PushAfter directly.
func (h *harness) state(repos ...state.Repo) *state.State {
	h.t.Helper()
	st := state.New()
	st.DefaultLocation = h.dataLoc
	st.Settings = state.Settings{
		IdleThreshold: state.Duration(time.Nanosecond),
		PushAfter:     state.Duration(time.Nanosecond),
		RepoOpTimeout: state.Duration(time.Minute),
	}
	for _, r := range repos {
		st.AddRepo(r)
	}
	return st
}

// jjClone makes a colocated jj clone of the origin at <dataLoc>/<relpath>. Unlike
// Fixture.JJClone it goes through the production vcs.Clone path, which vcstest
// cannot import.
func (h *harness) jjClone(relpath string) string {
	h.t.Helper()
	dest := filepath.Join(h.dataLoc, relpath)
	if err := vcs.Clone(context.Background(), h.Origin, dest); err != nil {
		h.t.Fatalf("jj clone %s: %v", relpath, err)
	}
	return dest
}

// extraOrigin creates and seeds a second bare origin so a test can register two
// repos with distinct origins (the convergent registry is keyed by origin, so two
// tracked repos cannot share one). It returns the new bare origin path.
func (h *harness) extraOrigin(name string) string {
	h.t.Helper()
	origin := filepath.Join(h.Root, name+".git")
	seed := filepath.Join(h.Root, name+"-seed")
	h.RunGit(h.Root, "init", "--bare", "-b", "main", origin)
	h.RunGit(h.Root, "clone", origin, seed)
	h.ConfigGit(seed)
	h.WriteFile(seed, "README.md", "hello "+name+"\n")
	h.RunGit(seed, "add", "README.md")
	h.RunGit(seed, "commit", "-q", "-m", "init")
	h.RunGit(seed, "push", "-q", "origin", "main")
	return origin
}

// localAhead writes real content into dest, commits it as a non-empty trunk commit
// with an empty @ on top, and moves the local main bookmark onto that commit,
// leaving local main strictly ahead of origin without pushing. The empty @ keeps
// the working copy disposable so InUse does not report it busy. It returns the
// local main commit id.
func (h *harness) localAhead(dest, name, content string) string {
	h.t.Helper()
	h.WriteFile(dest, name, content)
	h.RunJJ(dest, "commit", "-m", name)
	h.RunJJ(dest, "bookmark", "set", "main", "-r", "@-", "--ignore-working-copy")
	return h.localMain(dest)
}

// localMain resolves the local main bookmark's commit id via the colocated git ref.
func (h *harness) localMain(dest string) string {
	h.t.Helper()
	return strings.TrimSpace(h.RunGit(dest, "rev-parse", "main"))
}

// TestFailureMapsContentionToBusy proves a working-copy contention error from a
// repo op degrades to a busy skip retried next cycle, while any other error
// surfaces unchanged.
func TestFailureMapsContentionToBusy(t *testing.T) {
	res := Result{Relpath: "alpha"}

	contention := fmt.Errorf("jj new main: %w", errors.New("Internal error: Failed to check out commit 99366219: Concurrent checkout"))
	got := failure(res, contention)
	if got.Err != nil {
		t.Fatalf("contention Err = %v, want nil", got.Err)
	}
	if got.Outcome != OutcomeBusy {
		t.Fatalf("contention outcome = %q, want busy", got.Outcome)
	}
	if got.Reason != "working-copy contention" {
		t.Fatalf("contention reason = %q, want working-copy contention", got.Reason)
	}

	plain := errors.New("network unreachable")
	got = failure(res, plain)
	if !errors.Is(got.Err, plain) {
		t.Fatalf("plain Err = %v, want the original error", got.Err)
	}
	if got.Outcome != "" || got.Reason != "" {
		t.Fatalf("plain outcome/reason = %q/%q, want empty", got.Outcome, got.Reason)
	}
}

func resultFor(t *testing.T, results []Result, relpath string) Result {
	t.Helper()
	for _, res := range results {
		if res.Relpath == relpath {
			return res
		}
	}
	t.Fatalf("no result for relpath %q in %+v", relpath, results)
	return Result{}
}

func TestSyncAdvancesIdleRepo(t *testing.T) {
	h := newHarness(t)
	h.jjClone("alpha")
	want := h.AdvanceOrigin("v2")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Outcome != vcs.OutcomeAdvanced {
		t.Fatalf("alpha outcome = %q, want advanced", res.Outcome)
	}

	r, _ := vcs.Open(filepath.Join(h.dataLoc, "alpha"), "main")
	if h, _ := r.TrunkHash(context.Background()); h != want {
		t.Fatalf("alpha trunk hash = %q, want %q", h, want)
	}
}

func TestSyncBusyRepoSkippedAndIntact(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("beta")
	h.AdvanceOrigin("v2")
	// A real edit recorded by a genuine jj snapshot makes @ dirty -> busy. The
	// default 1ns idle threshold keeps the recency gate out of the way so the
	// dirty probe is what fires.
	h.WriteFile(dest, "WORK.txt", "in progress\n")
	h.RunJJ(dest, "status")
	st := h.state(state.Repo{Relpath: "beta", Origin: h.Origin, Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "beta")
	if res.Err != nil {
		t.Fatalf("beta err: %v", res.Err)
	}
	if res.Outcome != OutcomeBusy {
		t.Fatalf("beta outcome = %q, want busy", res.Outcome)
	}
	if res.Reason != "dirty working copy" {
		t.Fatalf("beta reason = %q, want dirty working copy", res.Reason)
	}
	if got := h.ReadFile(dest, "WORK.txt"); got != "in progress\n" {
		t.Fatalf("dirty change clobbered: %q", got)
	}
}

// TestSyncLockedRepoSkippedAndIntact proves a repo with a live git ref transaction
// is treated as busy and left untouched: the exact packed-refs.lock symptom that
// orphaned the reported commits now short-circuits at the InUse gate. Origin does
// not move even with trunk advanced and the push gate open.
func TestSyncLockedRepoSkippedAndIntact(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("beta")
	h.AdvanceOrigin("v2")

	lock := filepath.Join(dest, ".git", "packed-refs.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	st := h.state(state.Repo{Relpath: "beta", Origin: h.Origin, Trunk: "main"})
	originBefore := h.OriginMain()
	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	res := resultFor(t, results, "beta")
	if res.Err != nil {
		t.Fatalf("beta err: %v", res.Err)
	}
	if res.Outcome != OutcomeBusy {
		t.Fatalf("beta outcome = %q, want busy", res.Outcome)
	}
	if res.Reason != "git refs locked" {
		t.Fatalf("beta reason = %q, want git refs locked", res.Reason)
	}
	if got := h.OriginMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q under a held lock, want unchanged", originBefore, got)
	}
}

func TestSyncNoTrunkRepo(t *testing.T) {
	h := newHarness(t)
	h.JJInit(filepath.Join(h.dataLoc, "gamma"))
	st := h.state(state.Repo{Relpath: "gamma", Origin: "", Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "gamma")
	if res.Err != nil {
		t.Fatalf("gamma err: %v", res.Err)
	}
	if res.Outcome != OutcomeNoTrunk {
		t.Fatalf("gamma outcome = %q, want no-trunk", res.Outcome)
	}
}

func TestSyncRepoFilterSelectsOne(t *testing.T) {
	h := newHarness(t)
	h.jjClone("alpha")
	betaOrigin := h.extraOrigin("beta")
	if err := vcs.Clone(context.Background(), betaOrigin, filepath.Join(h.dataLoc, "beta")); err != nil {
		t.Fatalf("clone beta: %v", err)
	}
	h.AdvanceOrigin("v2")
	st := h.state(
		state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"},
		state.Repo{Relpath: "beta", Origin: betaOrigin, Trunk: "main"},
	)

	t.Run("by relpath", func(t *testing.T) {
		results, err := Sync(context.Background(), st, "beta", "")
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("len(results) = %d, want 1", len(results))
		}
		if results[0].Relpath != "beta" {
			t.Fatalf("selected %q, want beta", results[0].Relpath)
		}
	})

	t.Run("by abspath", func(t *testing.T) {
		abs := filepath.Join(h.dataLoc, "alpha")
		results, err := Sync(context.Background(), st, abs, "")
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(results) != 1 || results[0].Relpath != "alpha" {
			t.Fatalf("selected %+v, want single alpha", results)
		}
	})
}

func TestSyncUnknownFilterErrors(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})

	_, err := Sync(context.Background(), st, "nonexistent", "")
	if err == nil {
		t.Fatal("Sync with unknown filter returned nil error")
	}
	if !strings.Contains(err.Error(), "repo not registered: nonexistent") {
		t.Fatalf("err = %v, want 'repo not registered: nonexistent'", err)
	}
}

func TestSyncBrokenRepoDoesNotAbortOthers(t *testing.T) {
	h := newHarness(t)
	h.jjClone("alpha")
	want := h.AdvanceOrigin("v2")
	st := h.state(
		// missing is registered but absent on disk: its Open fails. It carries a
		// distinct origin so it and alpha are separate registry entries.
		state.Repo{Relpath: "missing", Origin: h.extraOrigin("missing"), Trunk: "main"},
		state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"},
	)

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	missing := resultFor(t, results, "missing")
	if missing.Err == nil {
		t.Fatal("missing repo: want Err, got nil")
	}
	alpha := resultFor(t, results, "alpha")
	if alpha.Err != nil {
		t.Fatalf("alpha err: %v (broken sibling aborted the run)", alpha.Err)
	}
	if alpha.Outcome != vcs.OutcomeAdvanced {
		t.Fatalf("alpha outcome = %q, want advanced", alpha.Outcome)
	}
	r, _ := vcs.Open(filepath.Join(h.dataLoc, "alpha"), "main")
	if got, _ := r.TrunkHash(context.Background()); got != want {
		t.Fatalf("alpha trunk hash = %q, want %q", got, want)
	}
}

func TestSyncRebasesGeneratedOnlyRepo(t *testing.T) {
	h := newHarness(t)
	h.SeedGenerated()
	dest := h.jjClone("delta")
	// Dirty the working copy with ONLY a generated edit, recorded by a real snapshot.
	h.WriteFile(dest, "build.gen", "generated local edit\n")
	h.RunJJ(dest, "status")
	// Advance trunk on a non-generated path so the generated edit rebases cleanly.
	want := h.AdvanceOrigin("v2")
	// Default short idle threshold: the clone ops are not seen as recent activity,
	// so the only thing the dirt gate sees is the generated-only working-copy edit.
	st := h.state(state.Repo{Relpath: "delta", Origin: h.Origin, Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "delta")
	if res.Err != nil {
		t.Fatalf("delta err: %v", res.Err)
	}
	if res.Outcome != vcs.OutcomeRebasedGenerated {
		t.Fatalf("delta outcome = %q, want rebased-generated", res.Outcome)
	}

	r, _ := vcs.Open(dest, "main")
	if got, _ := r.TrunkHash(context.Background()); got != want {
		t.Fatalf("delta trunk hash = %q, want %q", got, want)
	}
}

// TestSyncPushesQuietAheadRepo proves the positive path: a quiet repo whose local
// trunk is strictly ahead of an unmoved origin is fast-forward pushed, and origin
// lands exactly on the local main commit.
func TestSyncPushesQuietAheadRepo(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	wantMain := h.localAhead(dest, "feature.txt", "shipped locally\n")
	// Both gates open (IdleThreshold and PushAfter default to 1ns via h.state).
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Outcome != vcs.OutcomePushed {
		t.Fatalf("alpha outcome = %q, want pushed", res.Outcome)
	}
	if got := h.OriginMain(); got != wantMain {
		t.Fatalf("origin main = %q, want local main %q", got, wantMain)
	}
}

// TestSyncNoPushWhenRecentlyActive proves the quiet gate: an ahead repo that has
// been active within PushAfter is not pushed even though Advance succeeds.
func TestSyncNoPushWhenRecentlyActive(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	h.localAhead(dest, "feature.txt", "shipped locally\n")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})
	// IdleThreshold stays 1ns (Advance reaches the push check); PushAfter=1h makes
	// the just-created clone look recently active, closing the push gate.
	st.Settings.PushAfter = state.Duration(time.Hour)

	originBefore := h.OriginMain()
	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Outcome == vcs.OutcomePushed {
		t.Fatalf("alpha outcome = %q, want not pushed (recently active)", res.Outcome)
	}
	if got := h.OriginMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, got)
	}
}

// TestSyncNoPushWhenDiverged proves a diverged repo is never force-moved: local is
// ahead AND origin has independently advanced. Advance classifies the divergence,
// the diverged outcome fails the push gate, and origin stays put.
func TestSyncNoPushWhenDiverged(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	h.localAhead(dest, "feature.txt", "shipped locally\n")
	originBefore := h.AdvanceOrigin("v2")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// jj classifies a diverged (conflicted) bookmark structurally, like git: no
	// error, diverged, and crucially origin is not force-moved.
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("diverged repo: want no error (diverged decline like git), got %v", res.Err)
	}
	if res.Outcome != vcs.OutcomeDiverged {
		t.Fatalf("outcome = %q, want diverged (declined untouched)", res.Outcome)
	}
	if got := h.OriginMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q on divergence, want unchanged", originBefore, got)
	}
}

// TestSyncNoPushWhenDirty proves a busy repo short-circuits before push: a dirty
// non-generated working copy yields OutcomeBusy and leaves origin untouched.
func TestSyncNoPushWhenDirty(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	h.localAhead(dest, "feature.txt", "shipped locally\n")
	// A real edit recorded by a genuine jj snapshot makes @ dirty -> busy; the
	// default 1ns idle threshold leaves the dirty probe as the deciding gate.
	h.WriteFile(dest, "WORK.txt", "in progress\n")
	h.RunJJ(dest, "status")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})

	originBefore := h.OriginMain()
	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Outcome != OutcomeBusy {
		t.Fatalf("alpha outcome = %q, want busy", res.Outcome)
	}
	if got := h.OriginMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, got)
	}
}

// TestSyncNoPushWhenNotAhead proves an up-to-date repo with no local lead does not
// push: local main was never moved, so PushTrunk finds nothing to send.
func TestSyncNoPushWhenNotAhead(t *testing.T) {
	h := newHarness(t)
	h.jjClone("alpha")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.Origin, Trunk: "main"})

	originBefore := h.OriginMain()
	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Outcome == vcs.OutcomePushed {
		t.Fatalf("alpha outcome = %q, want not pushed (not ahead)", res.Outcome)
	}
	if got := h.OriginMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, got)
	}
}
