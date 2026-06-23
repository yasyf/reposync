package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

const jjTestConfig = `[user]
name = "Test User"
email = "test@example.com"
`

// harness is a temp-dir test rig: a real bare git origin, a seed clone used to
// publish new trunk commits, and a default_location into which repos are cloned.
type harness struct {
	t       *testing.T
	root    string
	origin  string // bare origin repo
	seed    string // plain-git clone used to push new commits to origin
	dataLoc string // default_location where tracked repos live
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	cfg := filepath.Join(root, "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte(jjTestConfig), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	requireJJ(t)

	h := &harness{
		t:       t,
		root:    root,
		origin:  filepath.Join(root, "origin.git"),
		seed:    filepath.Join(root, "seed"),
		dataLoc: filepath.Join(root, "data"),
	}
	if err := os.MkdirAll(h.dataLoc, 0o750); err != nil {
		t.Fatalf("mkdir data loc: %v", err)
	}
	h.runGit(root, "init", "--bare", "-b", "main", h.origin)
	h.runGit(root, "clone", h.origin, h.seed)
	h.configGit(h.seed)
	h.writeFile(h.seed, "README.md", "hello\n")
	h.runGit(h.seed, "add", "README.md")
	h.runGit(h.seed, "commit", "-q", "-m", "init")
	h.runGit(h.seed, "push", "-q", "origin", "main")
	return h
}

func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skipf("jj not installed: %v", err)
	}
	out, err := exec.Command("jj", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("jj --version failed: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "jj ") {
		t.Fatalf("unexpected jj --version output: %s", out)
	}
}

// state builds a *state.State pointed at this harness's default_location with a
// short idle threshold so freshly-cloned repos are not seen as recently active.
// PushAfter is likewise 1ns so the push gate treats a quiet repo as quiet; tests
// that need to suppress the push override st.Settings.PushAfter directly.
func (h *harness) state(repos ...state.Repo) *state.State {
	h.t.Helper()
	return &state.State{
		DefaultLocation: h.dataLoc,
		Repos:           repos,
		Settings: state.Settings{
			IdleThreshold: state.Duration(time.Nanosecond),
			PushAfter:     state.Duration(time.Nanosecond),
			RepoOpTimeout: state.Duration(time.Minute),
		},
	}
}

// jjClone makes a colocated jj clone of the origin at <dataLoc>/<relpath>.
func (h *harness) jjClone(relpath string) string {
	h.t.Helper()
	dest := filepath.Join(h.dataLoc, relpath)
	if err := vcs.Clone(context.Background(), h.origin, dest); err != nil {
		h.t.Fatalf("jj clone %s: %v", relpath, err)
	}
	return dest
}

// advanceOrigin pushes a new trunk commit and returns the new origin main hash.
func (h *harness) advanceOrigin(content string) string {
	h.t.Helper()
	cur := h.readFile(h.seed, "README.md")
	h.writeFile(h.seed, "README.md", cur+content+"\n")
	h.runGit(h.seed, "commit", "-aqm", content)
	h.runGit(h.seed, "push", "-q", "origin", "main")
	return h.originMain()
}

// localAhead writes real content into dest, commits it as a non-empty trunk commit
// with an empty @ on top, and moves the local main bookmark onto that commit,
// leaving local main strictly ahead of origin without pushing. The empty @ keeps
// the working copy disposable so InUse does not report it busy. It returns the
// local main commit id.
func (h *harness) localAhead(dest, name, content string) string {
	h.t.Helper()
	h.writeFile(dest, name, content)
	h.runJJ(dest, "commit", "-m", name)
	h.runJJ(dest, "bookmark", "set", "main", "-r", "@-", "--ignore-working-copy")
	return h.localMain(dest)
}

// localMain resolves the local main bookmark's commit id via the colocated git ref.
func (h *harness) localMain(dest string) string {
	h.t.Helper()
	return strings.TrimSpace(h.runGit(dest, "rev-parse", "main"))
}

// seedGenerated writes a .gitattributes marking *.gen as linguist-generated plus
// an initial build.gen, commits, and pushes both onto trunk via the seed clone.
// Call after newHarness and before cloning the work repo so both files are on
// trunk in the clone.
func (h *harness) seedGenerated() {
	h.t.Helper()
	h.writeFile(h.seed, ".gitattributes", "*.gen linguist-generated\n")
	h.writeFile(h.seed, "build.gen", "generated v1\n")
	h.runGit(h.seed, "add", ".gitattributes", "build.gen")
	h.runGit(h.seed, "commit", "-qm", "seed generated")
	h.runGit(h.seed, "push", "-q", "origin", "main")
}

func (h *harness) originMain() string {
	h.t.Helper()
	return strings.TrimSpace(h.runGit(h.root, "-C", h.origin, "rev-parse", "main"))
}

func (h *harness) configGit(dir string) {
	h.t.Helper()
	h.runGit(dir, "config", "user.name", "Test User")
	h.runGit(dir, "config", "user.email", "test@example.com")
}

func (h *harness) runGit(dir string, args ...string) string {
	h.t.Helper()
	//nolint:gosec // G204: test helper running git with test-controlled args against a temp repo.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *harness) runJJ(dir string, args ...string) string {
	h.t.Helper()
	//nolint:gosec // G204: test helper running jj with test-controlled args against a temp repo.
	cmd := exec.Command("jj", append([]string{"--repository", dir}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("jj %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *harness) writeFile(dir, name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatalf("write %s: %v", name, err)
	}
}

func (h *harness) readFile(dir, name string) string {
	h.t.Helper()
	//nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		h.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
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
	want := h.advanceOrigin("v2")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

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
	h.advanceOrigin("v2")
	// A real edit recorded by a genuine jj snapshot makes @ dirty -> busy.
	h.writeFile(dest, "WORK.txt", "in progress\n")
	h.runJJ(dest, "status")
	st := h.state(state.Repo{Relpath: "beta", Origin: h.origin, Trunk: "main"})
	st.Settings.IdleThreshold = state.Duration(time.Hour)

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "beta")
	if res.Err != nil {
		t.Fatalf("beta err: %v", res.Err)
	}
	if res.Outcome != vcs.OutcomeBusy {
		t.Fatalf("beta outcome = %q, want busy", res.Outcome)
	}
	if res.Reason != "dirty working copy" {
		t.Fatalf("beta reason = %q, want dirty working copy", res.Reason)
	}
	if got := h.readFile(dest, "WORK.txt"); got != "in progress\n" {
		t.Fatalf("dirty change clobbered: %q", got)
	}
}

func TestSyncNoTrunkRepo(t *testing.T) {
	h := newHarness(t)
	dest := filepath.Join(h.dataLoc, "gamma")
	//nolint:gosec // G204: test running jj against a test-controlled temp dest.
	cmd := exec.Command("jj", "git", "init", "--colocate", dest)
	cmd.Dir = h.root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jj git init --colocate: %v: %s", err, out)
	}
	st := h.state(state.Repo{Relpath: "gamma", Origin: "", Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "gamma")
	if res.Err != nil {
		t.Fatalf("gamma err: %v", res.Err)
	}
	if res.Outcome != vcs.OutcomeNoTrunk {
		t.Fatalf("gamma outcome = %q, want no-trunk", res.Outcome)
	}
}

func TestSyncRepoFilterSelectsOne(t *testing.T) {
	h := newHarness(t)
	h.jjClone("alpha")
	h.jjClone("beta")
	h.advanceOrigin("v2")
	st := h.state(
		state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"},
		state.Repo{Relpath: "beta", Origin: h.origin, Trunk: "main"},
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
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

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
	want := h.advanceOrigin("v2")
	st := h.state(
		// missing is registered but absent on disk: its Open fails.
		state.Repo{Relpath: "missing", Origin: h.origin, Trunk: "main"},
		state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"},
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
	h.seedGenerated()
	dest := h.jjClone("delta")
	// Dirty the working copy with ONLY a generated edit, recorded by a real snapshot.
	h.writeFile(dest, "build.gen", "generated local edit\n")
	h.runJJ(dest, "status")
	// Advance trunk on a non-generated path so the generated edit rebases cleanly.
	want := h.advanceOrigin("v2")
	// Default short idle threshold: the clone ops are not seen as recent activity,
	// so the only thing the dirt gate sees is the generated-only working-copy edit.
	st := h.state(state.Repo{Relpath: "delta", Origin: h.origin, Trunk: "main"})

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
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

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
	if got := h.originMain(); got != wantMain {
		t.Fatalf("origin main = %q, want local main %q", got, wantMain)
	}
}

// TestSyncNoPushWhenRecentlyActive proves the quiet gate: an ahead repo that has
// been active within PushAfter is not pushed even though Advance succeeds.
func TestSyncNoPushWhenRecentlyActive(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	h.localAhead(dest, "feature.txt", "shipped locally\n")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
	// IdleThreshold stays 1ns (Advance reaches the push check); PushAfter=1h makes
	// the just-created clone look recently active, closing the push gate.
	st.Settings.PushAfter = state.Duration(time.Hour)

	originBefore := h.originMain()
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
	if got := h.originMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, got)
	}
}

// TestSyncNoPushWhenDiverged proves a diverged repo is never force-moved: local is
// ahead AND origin has independently advanced. jj's conflicted-bookmark skip (and
// git's non-FF rejection) keep origin put even with the push gate open.
func TestSyncNoPushWhenDiverged(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	h.localAhead(dest, "feature.txt", "shipped locally\n")
	originBefore := h.advanceOrigin("v2")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Advance erroring into res.Err on the conflicted bookmark is pre-existing; the
	// invariant under test is that origin is not force-moved.
	_ = resultFor(t, results, "alpha")
	if got := h.originMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q on divergence, want unchanged", originBefore, got)
	}
}

// TestSyncNoPushWhenDirty proves a busy repo short-circuits before push: a dirty
// non-generated working copy yields OutcomeBusy and leaves origin untouched.
func TestSyncNoPushWhenDirty(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	h.localAhead(dest, "feature.txt", "shipped locally\n")
	// A real edit recorded by a genuine jj snapshot makes @ dirty -> busy.
	h.writeFile(dest, "WORK.txt", "in progress\n")
	h.runJJ(dest, "status")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
	st.Settings.IdleThreshold = state.Duration(time.Hour)

	originBefore := h.originMain()
	results, err := Sync(context.Background(), st, "", "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Outcome != vcs.OutcomeBusy {
		t.Fatalf("alpha outcome = %q, want busy", res.Outcome)
	}
	if got := h.originMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, got)
	}
}

// TestSyncNoPushWhenNotAhead proves an up-to-date repo with no local lead does not
// push: local main was never moved, so PushTrunk finds nothing to send.
func TestSyncNoPushWhenNotAhead(t *testing.T) {
	h := newHarness(t)
	h.jjClone("alpha")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	originBefore := h.originMain()
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
	if got := h.originMain(); got != originBefore {
		t.Fatalf("origin main moved from %q to %q, want unchanged", originBefore, got)
	}
}
