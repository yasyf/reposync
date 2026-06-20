package rpc

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
)

// harness is a temp-dir test rig: a real bare git origin, a seed clone used to
// publish new trunk commits, and a default_location into which repos are checked
// out with plain git (no jj needed — vcs detects the backing VCS per repo).
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
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))

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

// gitClone makes a plain-git checkout of the origin at <dataLoc>/<relpath>.
func (h *harness) gitClone(relpath string) string {
	h.t.Helper()
	dest := filepath.Join(h.dataLoc, relpath)
	h.runGit(h.root, "clone", "-q", h.origin, dest)
	h.configGit(dest)
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

func (h *harness) state(repos ...state.Repo) *state.State {
	h.t.Helper()
	return &state.State{
		DefaultLocation: h.dataLoc,
		Repos:           repos,
		Settings: state.Settings{
			IdleThreshold: state.Duration(time.Nanosecond),
			RepoOpTimeout: state.Duration(time.Minute),
		},
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

func (h *harness) readFile(dir, name string) string {
	h.t.Helper()
	//nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		h.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// serve starts srv on a fresh unix listener with a short socket path (well under
// the macOS sun_path limit) and returns the socket path. The listener and server
// are torn down when the test ends.
func serve(t *testing.T, srv *Server) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rpcsock")
	if err != nil {
		t.Fatalf("mkdir sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, ln); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = ln.Close()
	})
	return sock
}

func TestSyncAdvances(t *testing.T) {
	h := newHarness(t)
	dest := h.gitClone("app")
	want := h.advanceOrigin("second")

	st := h.state(state.Repo{Relpath: "app", Origin: h.origin, Trunk: "main"})
	srv := NewServer(func() (*state.State, error) { return st, nil })
	sock := serve(t, srv)

	resp, err := Sync(context.Background(), sock, "app", "")
	if err != nil {
		t.Fatalf("rpc sync: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("response error: %s", resp.Err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d: %+v", len(resp.Results), resp.Results)
	}
	r := resp.Results[0]
	if r.Err != "" {
		t.Fatalf("result error: %s", r.Err)
	}
	if r.Relpath != "app" {
		t.Errorf("relpath = %q, want app", r.Relpath)
	}
	if r.Outcome != "advanced" {
		t.Errorf("outcome = %q, want advanced", r.Outcome)
	}
	got := strings.TrimSpace(h.runGit(dest, "rev-parse", "origin/main"))
	if got != want {
		t.Errorf("local origin/main = %q, want %q", got, want)
	}
}

func TestNoDeadlockConcurrentSyncReconcile(t *testing.T) {
	h := newHarness(t)
	h.gitClone("app")
	h.advanceOrigin("second")

	st := h.state(state.Repo{Relpath: "app", Origin: h.origin, Trunk: "main"})
	srv := NewServer(func() (*state.State, error) { return st, nil })
	sock := serve(t, srv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := Sync(context.Background(), sock, "", ""); err != nil {
				t.Errorf("rpc sync: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := Reconcile(context.Background(), sock); err != nil {
				t.Errorf("rpc reconcile: %v", err)
			}
		}()
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: concurrent sync+reconcile did not both return")
	}
}

func TestClientFailsLoudOnMissingDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Sync(ctx, "/nonexistent/x.sock", "r", ""); err == nil {
		t.Fatal("want error dialing missing daemon, got nil")
	}
}

func TestUnknownMethod(t *testing.T) {
	h := newHarness(t)
	st := h.state()
	srv := NewServer(func() (*state.State, error) { return st, nil })
	sock := serve(t, srv)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(`{"method":"bogus"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "unknown method") {
		t.Errorf("response = %q, want it to mention unknown method", got)
	}
}
