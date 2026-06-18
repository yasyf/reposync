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
	if err := os.MkdirAll(h.dataLoc, 0o755); err != nil {
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
func (h *harness) state(repos ...state.Repo) *state.State {
	h.t.Helper()
	return &state.State{
		DefaultLocation: h.dataLoc,
		Repos:           repos,
		Settings: state.Settings{
			IdleThreshold: state.Duration(time.Nanosecond),
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

func TestSyncNeverPushes(t *testing.T) {
	h := newHarness(t)
	dest := h.jjClone("alpha")
	// Make local main ahead of origin by committing locally (no push).
	h.runJJ(dest, "describe", "-m", "local ahead", "--ignore-working-copy")
	h.runJJ(dest, "bookmark", "set", "main", "-r", "@", "--ignore-working-copy")
	h.advanceOrigin("v2")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	originBefore := h.originMain()
	if _, err := Sync(context.Background(), st, "", ""); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if originBefore != h.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, h.originMain())
	}
}
