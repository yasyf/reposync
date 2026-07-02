// Package vcstest provides a temp-dir fixture over real git and jj binaries
// for tests that exercise the vcs layer: a bare origin, a seed clone that
// publishes trunk commits, and helpers to build git and colocated jj clones.
package vcstest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const jjTestConfig = `[user]
name = "Test User"
email = "test@example.com"
`

// Fixture is a temp-dir test harness over real git and jj binaries: a bare git
// origin plus a seed clone used to publish new trunk commits.
type Fixture struct {
	t      *testing.T
	Root   string
	Origin string // path to the bare origin repo
	Seed   string // a plain-git clone used to push new commits to origin
}

// New builds a Fixture rooted in a symlink-resolved temp dir: it points
// JJ_CONFIG at a test identity, requires a working jj, and seeds a bare origin
// with an initial commit published from the seed clone.
func New(t *testing.T) *Fixture {
	t.Helper()
	// EvalSymlinks keeps path equality on macOS, where t.TempDir() sits under
	// the /var -> /private/var symlink.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	cfg := filepath.Join(root, "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte(jjTestConfig), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	RequireJJ(t)

	f := &Fixture{
		t:      t,
		Root:   root,
		Origin: filepath.Join(root, "origin.git"),
		Seed:   filepath.Join(root, "seed"),
	}
	f.RunGit(root, "init", "--bare", "-b", "main", f.Origin)
	f.RunGit(root, "clone", f.Origin, f.Seed)
	f.ConfigGit(f.Seed)
	f.WriteFile(f.Seed, "README.md", "hello\n")
	f.RunGit(f.Seed, "add", "README.md")
	f.RunGit(f.Seed, "commit", "-q", "-m", "init")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	return f
}

// RequireJJ proves jj actually works in the test environment before any assertions.
func RequireJJ(t *testing.T) {
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

// GitClone makes a plain-git clone of the origin at dest and configures identity.
func (f *Fixture) GitClone(dest string) string {
	f.t.Helper()
	f.RunGit(f.Root, "clone", f.Origin, dest)
	f.ConfigGit(dest)
	return dest
}

// JJClone makes a colocated jj clone of the origin at dest. It shells jj
// directly rather than calling vcs.Clone: vcstest must not import vcs, whose
// in-package tests import vcstest (an import cycle in the test binary).
func (f *Fixture) JJClone(dest string) string {
	f.t.Helper()
	//nolint:gosec // G204: test helper running jj against a test-controlled temp dest.
	cmd := exec.Command("jj", "git", "clone", "--colocate", f.Origin, dest)
	cmd.Dir = f.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("jj git clone --colocate %s: %v: %s", dest, err, out)
	}
	return dest
}

// JJInit creates a remoteless colocated jj repo at dest.
func (f *Fixture) JJInit(dest string) string {
	f.t.Helper()
	//nolint:gosec // G204: test helper running jj against a test-controlled temp dest.
	cmd := exec.Command("jj", "git", "init", "--colocate", dest)
	cmd.Dir = f.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("jj git init --colocate %s: %v: %s", dest, err, out)
	}
	return dest
}

// AdvanceOrigin appends content to README via the seed clone and pushes a new
// commit to origin, returning the new origin main commit hash.
func (f *Fixture) AdvanceOrigin(content string) string {
	f.t.Helper()
	cur := f.ReadFile(f.Seed, "README.md")
	f.WriteFile(f.Seed, "README.md", cur+content+"\n")
	f.RunGit(f.Seed, "commit", "-aqm", content)
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	return f.OriginMain()
}

// SeedGenerated writes a .gitattributes marking *.gen as linguist-generated plus
// an initial build.gen, commits, and pushes both onto trunk via the seed clone.
func (f *Fixture) SeedGenerated() {
	f.t.Helper()
	f.WriteFile(f.Seed, ".gitattributes", "*.gen linguist-generated\n")
	f.WriteFile(f.Seed, "build.gen", "generated v1\n")
	f.RunGit(f.Seed, "add", ".gitattributes", "build.gen")
	f.RunGit(f.Seed, "commit", "-qm", "seed generated")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
}

// AdvanceOriginPath writes content to path via the seed clone and pushes a new
// commit to origin, returning the new origin main commit hash.
func (f *Fixture) AdvanceOriginPath(path, content string) string {
	f.t.Helper()
	f.WriteFile(f.Seed, path, content)
	f.RunGit(f.Seed, "add", path)
	f.RunGit(f.Seed, "commit", "-qm", "advance "+path)
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	return f.OriginMain()
}

// OriginMain returns the bare origin's current main commit hash.
func (f *Fixture) OriginMain() string {
	f.t.Helper()
	return strings.TrimSpace(f.RunGit(f.Root, "-C", f.Origin, "rev-parse", "main"))
}

// SnapshotJJ forces a jj snapshot of the working copy (without --ignore-working-copy),
// mimicking the real user activity that records edits the poller later observes.
func (f *Fixture) SnapshotJJ(repo string) {
	f.t.Helper()
	f.RunJJ(repo, "status")
}

// JJOpHead returns the current jj op log head id, for asserting a probe
// appended no operation.
func (f *Fixture) JJOpHead(repo string) string {
	f.t.Helper()
	return strings.TrimSpace(f.RunJJ(repo, "op", "log", "--no-graph", "--ignore-working-copy", "-n", "1", "-T", `id.short() ++ "\n"`))
}

// JJSnapshotOps counts the "snapshot working copy" operations in the op log,
// for asserting a code path never snapshotted even when it records other ops
// (e.g. a fetch).
func (f *Fixture) JJSnapshotOps(repo string) int {
	f.t.Helper()
	out := f.RunJJ(repo, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `description.first_line() ++ "\n"`)
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "snapshot working copy" {
			n++
		}
	}
	return n
}

// ConfigGit sets the test git identity (user.name and user.email) in dir.
func (f *Fixture) ConfigGit(dir string) {
	f.t.Helper()
	f.RunGit(dir, "config", "user.name", "Test User")
	f.RunGit(dir, "config", "user.email", "test@example.com")
}

// RunGit runs git with args in dir, failing the test on any error.
func (f *Fixture) RunGit(dir string, args ...string) string {
	f.t.Helper()
	//nolint:gosec // G204: test helper running git with test-controlled args against a temp repo.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// RunJJ runs jj with args against the repo at dir, failing the test on any error.
func (f *Fixture) RunJJ(dir string, args ...string) string {
	f.t.Helper()
	//nolint:gosec // G204: test helper running jj with test-controlled args against a temp repo.
	cmd := exec.Command("jj", append([]string{"--repository", dir}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("jj %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// RunJJConflicts returns the conflict list from `jj resolve --list`, treating the
// no-conflicts case (which exits non-zero with "No conflicts found" on stderr) as
// an empty list rather than a fatal error.
func (f *Fixture) RunJJConflicts(dir string) string {
	f.t.Helper()
	//nolint:gosec // G204: test helper running jj against a test-controlled temp repo.
	cmd := exec.Command("jj", "--repository", dir, "resolve", "--list")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "No conflicts found") {
			return ""
		}
		f.t.Fatalf("jj resolve --list: %v: %s", err, stderr.String())
	}
	return stdout.String()
}

// WriteFile writes content to name under dir, failing the test on any error.
func (f *Fixture) WriteFile(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

// ReadFile returns the content of name under dir, failing the test on any error.
func (f *Fixture) ReadFile(dir, name string) string {
	f.t.Helper()
	//nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		f.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// FileExists reports whether name exists under dir.
func (f *Fixture) FileExists(dir, name string) bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}
