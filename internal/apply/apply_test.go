package apply

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	st := &state.State{
		DefaultLocation: h.dataLoc,
		Repos:           repos,
		Settings: state.Settings{
			IdleThreshold: state.Duration(time.Nanosecond),
			RepoOpTimeout: state.Duration(time.Minute),
		},
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

// fakeRunner records every Local/SSH call and returns "", nil so PropagateRepo
// and RemoteReconcile succeed without real ssh. Copied from host_test's shape.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRunner) Local(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "local "+strings.TrimSpace(name+" "+strings.Join(args, " ")))
	return "", nil
}

func (f *fakeRunner) SSH(_ context.Context, target, remoteCmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ssh "+target+": "+remoteCmd)
	return "", nil
}

func (f *fakeRunner) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
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
	for _, r := range st.Repos {
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

	results, err := Repos(context.Background(), &fakeRunner{}, sel)
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

func TestApplyReposEnablePropagatesToPeers(t *testing.T) {
	h := newHarness(t)
	h.seedState()
	if _, err := state.Update(context.Background(), func(s *state.State) error {
		s.UpsertHost("yasyf@peer")
		return nil
	}); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	r := &fakeRunner{}
	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "alpha", Origin: h.origin, Kind: "git"},
		},
	}
	if _, err := Repos(context.Background(), r, sel); err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	calls := r.recorded()
	var addRemote, rpcReconcile int
	for _, c := range calls {
		if strings.Contains(c, "ssh yasyf@peer:") && strings.Contains(c, "add-remote") {
			addRemote++
		}
		if strings.Contains(c, "ssh yasyf@peer:") && strings.Contains(c, "rpc reconcile") {
			rpcReconcile++
		}
	}
	if addRemote != 1 {
		t.Fatalf("got %d add-remote calls to peer, want 1: %v", addRemote, calls)
	}
	if rpcReconcile != 1 {
		t.Fatalf("got %d rpc reconcile calls to peer, want 1: %v", rpcReconcile, calls)
	}
}

func TestApplyReposLocalOnlyNotPropagated(t *testing.T) {
	h := newHarness(t)
	h.seedState()
	if _, err := state.Update(context.Background(), func(s *state.State) error {
		s.UpsertHost("yasyf@peer")
		return nil
	}); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	r := &fakeRunner{}
	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "local", Origin: "", Kind: "git", LocalOnly: true},
		},
	}
	if _, err := Repos(context.Background(), r, sel); err != nil {
		t.Fatalf("ApplyRepos: %v", err)
	}

	for _, c := range r.recorded() {
		if strings.HasPrefix(c, "ssh ") {
			t.Fatalf("local-only enable triggered a peer push: %q", c)
		}
	}
	if _, ok := loadRepo(t, "local"); !ok {
		t.Fatal("local-only repo not persisted")
	}
}

func TestApplyReposDisableUntracksButKeepsCheckout(t *testing.T) {
	h := newHarness(t)
	h.seedState(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	// Bring the pre-tracked repo on disk via reconcile, then confirm it is there.
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, err := reconcile.Reconcile(context.Background(), st); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}
	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) && !h.exists(filepath.Join(dest, ".git")) {
		t.Fatal("precondition failed: alpha checkout not present before disable")
	}

	sel := RepoSelection{Disable: []string{"alpha"}}
	if _, err := Repos(context.Background(), &fakeRunner{}, sel); err != nil {
		t.Fatalf("ApplyRepos disable: %v", err)
	}

	if _, ok := loadRepo(t, "alpha"); ok {
		t.Fatal("alpha still tracked in persisted state after disable")
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
	if _, err := reconcile.Reconcile(context.Background(), st); err != nil {
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
	results, err := Repos(context.Background(), &fakeRunner{}, sel)
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

func TestApplyReposPropagateFailureKeepsResults(t *testing.T) {
	h := newHarness(t)
	h.seedState()
	if _, err := state.Update(context.Background(), func(s *state.State) error {
		s.UpsertHost("yasyf@peer")
		return nil
	}); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	r := &failingRunner{}
	sel := RepoSelection{
		Enable: []discover.Candidate{
			{Relpath: "alpha", Origin: h.origin, Kind: "git"},
		},
	}
	results, err := Repos(context.Background(), r, sel)
	if err == nil {
		t.Fatal("expected a joined propagation error when the peer push fails")
	}
	// The reconcile results must survive a propagation failure.
	res := resultFor(t, results, "alpha")
	if res.Action != reconcile.ActionCloned || res.Err != nil {
		t.Fatalf("results discarded on propagate failure: %+v", res)
	}
}

// failingRunner fails every SSH call so the propagation/peer-reconcile error
// path is exercised; Local is unused here.
type failingRunner struct{}

func (failingRunner) Local(_ context.Context, _ string, _ ...string) (string, error) {
	return "", nil
}

func (failingRunner) SSH(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("connection refused")
}
