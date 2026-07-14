package apply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

const jjTestConfig = `[user]
name = "Test User"
email = "test@example.com"
`

// harness is a temp-dir test rig: a real bare git origin, a seed clone used to
// publish trunk commits, and a default_location into which repos are reconciled.
// XDG_CONFIG_HOME points at a temp dir so state persists for state.Load.
type harness struct {
	t       *testing.T
	root    string
	origin  string
	seed    string
	dataLoc string
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

// seedState persists a baseline state to the temp XDG dir so state.Load sees the
// configured default_location and any pre-tracked repos.
func (h *harness) seedState(repos ...state.Repo) {
	h.t.Helper()
	st := state.New()
	st.DefaultLocation = h.dataLoc
	st.Settings = state.Settings{
		IdleThreshold: state.Duration(time.Nanosecond),
		RepoOpTimeout: state.Duration(time.Minute),
	}
	for _, r := range repos {
		st.AddRepo(r)
	}
	if err := st.Save(); err != nil {
		h.t.Fatalf("seed state: %v", err)
	}
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

func (h *harness) writeFile(dir, name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatalf("write %s: %v", name, err)
	}
}

func (h *harness) exists(path string) bool {
	h.t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func resultFor(t *testing.T, results []reconcile.Result, relpath string) reconcile.Result {
	t.Helper()
	for _, res := range results {
		if res.Relpath == relpath {
			return res
		}
	}
	t.Fatalf("no result for relpath %q in %+v", relpath, results)
	return reconcile.Result{}
}

func loadRepo(t *testing.T, relpath string) (state.Repo, bool) {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load persisted state: %v", err)
	}
	for _, r := range st.AllRepos() {
		if r.Relpath == relpath {
			return r, true
		}
	}
	return state.Repo{}, false
}

func TestApplyReposEnableClonesAndPersists(t *testing.T) {
	h := newHarness(t)
	h.seedState()

	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "alpha", Origin: h.origin, Kind: "git"},
		},
	}

	results, err := Repos(context.Background(), sel)
	if err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Action != reconcile.ActionCloned {
		t.Fatalf("alpha action = %q, want %q", res.Action, reconcile.ActionCloned)
	}

	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) && !h.exists(filepath.Join(dest, ".git")) {
		t.Fatal("alpha checkout missing: neither .jj nor .git present after enable")
	}

	repo, ok := loadRepo(t, "alpha")
	if !ok {
		t.Fatal("alpha not present in persisted state after enable")
	}
	if repo.Origin != h.origin {
		t.Fatalf("persisted origin = %q, want %q", repo.Origin, h.origin)
	}
	if repo.Trunk != "main" {
		t.Fatalf("persisted trunk = %q, want %q", repo.Trunk, "main")
	}
	if repo.LocalOnly {
		t.Fatal("persisted alpha marked local-only, want tracked-with-origin")
	}
}

// TestApplyReposEnableAddsPropagatingEntry proves enable adds the repo to the
// origin-keyed convergent registry (present, not local-only) — the entry a peer
// pull-merges. apply does no peer push; convergence is the peers' job.
func TestApplyReposEnableAddsPropagatingEntry(t *testing.T) {
	h := newHarness(t)
	h.seedState()

	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "alpha", Origin: h.origin, Kind: "git"},
		},
	}
	if _, err := Repos(context.Background(), sel); err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	entry, ok := st.Repos[h.origin]
	if !ok {
		t.Fatalf("enabled repo not in propagating registry keyed by origin: %v", st.Repos)
	}
	if !entry.Present() {
		t.Fatal("enabled repo entry not present (added_at must beat removed_at)")
	}
	if entry.Value.Relpath != "alpha" || entry.Value.LocalOnly {
		t.Fatalf("registry payload = %+v, want relpath=alpha local_only=false", entry.Value)
	}
}

// TestApplyReposThreadsNoEnvSync proves an enabled candidate's env opt-out reaches the
// persisted registry payload, the flag `repo add --no-env-sync` carries.
func TestApplyReposThreadsNoEnvSync(t *testing.T) {
	h := newHarness(t)
	h.seedState()

	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "alpha", Origin: h.origin, Kind: "git", NoEnvSync: true},
		},
	}
	if _, err := Repos(context.Background(), sel); err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	entry, ok := st.Repos[h.origin]
	if !ok {
		t.Fatalf("enabled repo not in propagating registry: %v", st.Repos)
	}
	if !entry.Value.NoEnvSync {
		t.Fatalf("apply did not thread NoEnvSync into the registry: %+v", entry.Value)
	}
}

func TestApplyReposLocalOnlyStaysLocal(t *testing.T) {
	h := newHarness(t)
	h.seedState()

	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "local", Origin: "", Kind: "git", LocalOnly: true},
		},
	}
	if _, err := Repos(context.Background(), sel); err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	// A local-only repo lives in the local registry keyed by relpath, never in the
	// propagating one — so it is excluded from what peers pull-merge.
	if len(st.Repos) != 0 {
		t.Fatalf("local-only repo leaked into the propagating registry: %v", st.Repos)
	}
	e, ok := st.LocalRepos["local"]
	if !ok || !e.Present() {
		t.Fatalf("local-only repo not present in the local registry: %v", st.LocalRepos)
	}
}

func TestApplyReposDisableTombstonesAndKeepsCheckout(t *testing.T) {
	h := newHarness(t)
	h.seedState(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	// Bring the pre-tracked repo on disk via reconcile, then confirm it is there.
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, err := reconcile.Reconcile(context.Background(), st, ""); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}
	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) && !h.exists(filepath.Join(dest, ".git")) {
		t.Fatal("precondition failed: alpha checkout not present before disable")
	}

	sel := RepoSelection{Disable: []string{"alpha"}}
	if _, err := Repos(context.Background(), sel); err != nil {
		t.Fatalf("ApplyRepos disable: %v", err)
	}

	// Disable tombstones the registry entry (so the removal propagates) but keeps the
	// entry and never touches the on-disk checkout.
	if _, ok := loadRepo(t, "alpha"); ok {
		t.Fatal("alpha still present (untombstoned) in persisted state after disable")
	}
	reloaded, err := state.Load()
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	entry, ok := reloaded.Repos[h.origin]
	if !ok {
		t.Fatal("disable dropped the registry entry instead of tombstoning it: removal would not propagate")
	}
	if entry.Present() {
		t.Fatal("disabled repo still present, want tombstoned")
	}
	if entry.Removed <= entry.Added {
		t.Fatalf("tombstone not later than add: added=%d removed=%d", entry.Added, entry.Removed)
	}
	if !h.exists(dest) {
		t.Fatal("disable deleted the on-disk checkout dir")
	}
	if !h.exists(filepath.Join(dest, ".jj")) && !h.exists(filepath.Join(dest, ".git")) {
		t.Fatal("disable gutted the checkout: neither .jj nor .git remains")
	}
}

func TestApplyReposReconcilesOnlyEnabledSubset(t *testing.T) {
	h := newHarness(t)
	// Pre-track a repo and bring it on disk so it would surface as "present" if
	// apply reconciled the whole registered set instead of just the new repo.
	h.seedState(state.Repo{Relpath: "beta", Origin: h.origin, Trunk: "main"})
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load seeded state: %v", err)
	}
	if _, err := reconcile.Reconcile(context.Background(), st, ""); err != nil {
		t.Fatalf("seed reconcile of pre-tracked repo: %v", err)
	}
	if !h.exists(filepath.Join(h.dataLoc, "beta", ".jj")) {
		t.Fatal("precondition: pre-tracked beta not present on disk")
	}

	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "alpha", Origin: h.origin, Kind: "git"},
		},
	}
	results, err := Repos(context.Background(), sel)
	if err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly one (the enabled repo)", results)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil || res.Action != reconcile.ActionCloned {
		t.Fatalf("alpha result = %+v, want cloned without error", res)
	}
	for _, r := range results {
		if r.Relpath == "beta" {
			t.Fatalf("apply reconciled the pre-tracked beta repo: %+v", results)
		}
	}
}
