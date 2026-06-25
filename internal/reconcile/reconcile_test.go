package reconcile

import (
	"context"
	"fmt"
	"net"
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
// publish trunk commits, and a default_location into which repos are reconciled.
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

func (h *harness) state(repos ...state.Repo) *state.State {
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
	return st
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

func (h *harness) readFile(dir, name string) string {
	h.t.Helper()
	//nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		h.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func (h *harness) exists(path string) bool {
	h.t.Helper()
	_, err := os.Stat(path)
	return err == nil
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

func TestReconcileClonesAbsentRepo(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("alpha action = %q, want cloned", res.Action)
	}

	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) {
		t.Fatal(".jj missing: clone not colocated jj")
	}
	if !h.exists(filepath.Join(dest, ".git")) {
		t.Fatal(".git missing: clone lacks git backing")
	}
	r, err := vcs.Open(dest, "main")
	if err != nil {
		t.Fatalf("open cloned repo: %v", err)
	}
	origin, err := r.Origin(context.Background())
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	if origin != h.origin {
		t.Fatalf("origin = %q, want %q", origin, h.origin)
	}
	// Temp staging directory is cleaned up.
	if h.exists(filepath.Join(h.dataLoc, TmpDirName)) {
		t.Fatal(".reposync-tmp left behind after reconcile")
	}
}

func TestReconcileNestedRelpathClone(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "Forge/private-ai", Origin: h.origin, Trunk: "main"})

	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := resultFor(t, results, "Forge/private-ai")
	if res.Err != nil {
		t.Fatalf("nested err: %v", res.Err)
	}
	dest := filepath.Join(h.dataLoc, "Forge", "private-ai")
	if !h.exists(filepath.Join(dest, ".jj")) {
		t.Fatal("nested clone .jj missing")
	}
}

func TestReconcilePresentRepoNotRecloned(t *testing.T) {
	h := newHarness(t)
	dest := filepath.Join(h.dataLoc, "alpha")
	if err := vcs.Clone(context.Background(), h.origin, dest); err != nil {
		t.Fatalf("seed clone: %v", err)
	}
	// A sentinel file proves the present checkout is untouched (not re-cloned).
	h.writeFile(dest, "SENTINEL.txt", "do not clobber\n")
	identityBefore := strings.TrimSpace(h.runGit(dest, "-C", dest, "rev-parse", "HEAD"))

	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha err: %v", res.Err)
	}
	if res.Action != ActionPresent {
		t.Fatalf("alpha action = %q, want present", res.Action)
	}
	if !h.exists(filepath.Join(dest, "SENTINEL.txt")) {
		t.Fatal("present repo was re-cloned: sentinel gone")
	}
	if got := h.readFile(dest, "SENTINEL.txt"); got != "do not clobber\n" {
		t.Fatalf("sentinel changed to %q", got)
	}
	identityAfter := strings.TrimSpace(h.runGit(dest, "-C", dest, "rev-parse", "HEAD"))
	if identityBefore != identityAfter {
		t.Fatalf("present repo HEAD moved from %q to %q", identityBefore, identityAfter)
	}
}

func TestReconcileSkipsLocalOnly(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "local", Origin: h.origin, Trunk: "main", LocalOnly: true})

	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := resultFor(t, results, "local")
	if res.Err != nil {
		t.Fatalf("local err: %v", res.Err)
	}
	if res.Action != ActionSkippedLocalOnly {
		t.Fatalf("action = %q, want skipped-local-only", res.Action)
	}
	if h.exists(filepath.Join(h.dataLoc, "local")) {
		t.Fatal("local-only repo was cloned")
	}
}

func TestReconcileSkipsNoOrigin(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "noorigin", Origin: "", Trunk: "main"})

	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := resultFor(t, results, "noorigin")
	if res.Err != nil {
		t.Fatalf("noorigin err: %v", res.Err)
	}
	if res.Action != ActionSkippedNoOrigin {
		t.Fatalf("action = %q, want skipped-no-origin", res.Action)
	}
	if h.exists(filepath.Join(h.dataLoc, "noorigin")) {
		t.Fatal("no-origin repo was cloned")
	}
}

func TestReconcileNonRepoDirCollisionNotOverwritten(t *testing.T) {
	h := newHarness(t)
	dest := filepath.Join(h.dataLoc, "alpha")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatalf("mkdir collision dir: %v", err)
	}
	// A non-repo file the clone must not destroy.
	h.writeFile(dest, "PRECIOUS.txt", "keep me\n")

	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := resultFor(t, results, "alpha")
	if res.Err == nil {
		t.Fatal("collision: want Err on rename over non-repo dir, got nil")
	}
	// The pre-existing dir is untouched: not a repo, file intact.
	if h.exists(filepath.Join(dest, ".jj")) || h.exists(filepath.Join(dest, ".git")) {
		t.Fatal("collision dir was overwritten with a repo")
	}
	if got := h.readFile(dest, "PRECIOUS.txt"); got != "keep me\n" {
		t.Fatalf("collision dir file changed to %q", got)
	}
	if h.exists(filepath.Join(h.dataLoc, TmpDirName)) {
		t.Fatal(".reposync-tmp left behind after collision")
	}
}

func TestReconcileIdempotent(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	first, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if resultFor(t, first, "alpha").Action != ActionCloned {
		t.Fatalf("first action = %q, want cloned", resultFor(t, first, "alpha").Action)
	}

	second, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	res := resultFor(t, second, "alpha")
	if res.Err != nil {
		t.Fatalf("second run err: %v", res.Err)
	}
	if res.Action != ActionPresent {
		t.Fatalf("second action = %q, want present", res.Action)
	}
}

// blackHole opens a loopback listener that accepts connections and holds them
// open without ever writing, so a clone over an ssh:// origin pointed at it hangs
// at the protocol handshake until the per-op deadline kills it. The accept
// goroutine owns every connection; cleanup closes the listener (unblocking
// Accept) and waits for the goroutine to drop the parked conns.
func blackHole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen black-hole: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c) // park it; never read or write
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

func TestReposTimesOutAndReleasesLock(t *testing.T) {
	h := newHarness(t)
	addr := blackHole(t)
	origin := fmt.Sprintf("ssh://git@%s/blackhole.git", addr)

	st := h.state(state.Repo{Relpath: "wedged", Origin: origin, Trunk: "main"})
	st.Settings.RepoOpTimeout = state.Duration(500 * time.Millisecond)

	done := make(chan []Result, 1)
	go func() {
		res, err := Repos(context.Background(), st, st.AllRepos())
		if err != nil {
			t.Errorf("Repos returned a top-level error: %v", err)
		}
		done <- res
	}()

	select {
	case results := <-done:
		res := resultFor(t, results, "wedged")
		if res.Err == nil {
			t.Fatal("wedged clone: want a deadline error, got nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Repos did not honor RepoOpTimeout on a black-hole origin")
	}

	// The per-op deadline must have released the flock: a fresh acquire is immediate.
	acquired := make(chan error, 1)
	go func() {
		acquired <- state.WithLock(context.Background(), func() error { return nil })
	}()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("WithLock after timed-out reconcile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flock not released after a timed-out reconcile")
	}
}

func TestReconcileReleasesLock(t *testing.T) {
	h := newHarness(t)
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	if _, err := Reconcile(context.Background(), st, ""); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	// If the flock were not released, this second call would deadlock; the test
	// timeout would catch it. Completing proves the lock was released.
	done := make(chan error, 1)
	go func() { _, err := Reconcile(context.Background(), st, ""); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Reconcile after lock release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second Reconcile blocked: reconcile lock was not released")
	}
}
