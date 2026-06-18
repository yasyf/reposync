package vcs

import (
	"bytes"
	"context"
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

// fixture is a temp-dir test harness over real git and jj binaries: a bare git
// origin plus a seed clone used to publish new trunk commits.
type fixture struct {
	t      *testing.T
	root   string
	origin string // path to the bare origin repo
	seed   string // a plain-git clone used to push new commits to origin
}

func newFixture(t *testing.T) *fixture {
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
	requireJJ(t)

	f := &fixture{
		t:      t,
		root:   root,
		origin: filepath.Join(root, "origin.git"),
		seed:   filepath.Join(root, "seed"),
	}
	f.runGit(root, "init", "--bare", "-b", "main", f.origin)
	f.runGit(root, "clone", f.origin, f.seed)
	f.configGit(f.seed)
	f.writeFile(f.seed, "README.md", "hello\n")
	f.runGit(f.seed, "add", "README.md")
	f.runGit(f.seed, "commit", "-q", "-m", "init")
	f.runGit(f.seed, "push", "-q", "origin", "main")
	return f
}

// requireJJ proves jj actually works in the test environment before any assertions.
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

// gitClone makes a plain-git clone of the origin at dest and configures identity.
func (f *fixture) gitClone(dest string) string {
	f.t.Helper()
	f.runGit(f.root, "clone", f.origin, dest)
	f.configGit(dest)
	return dest
}

// jjClone makes a colocated jj clone of the origin at dest.
func (f *fixture) jjClone(dest string) string {
	f.t.Helper()
	if err := Clone(context.Background(), f.origin, dest); err != nil {
		f.t.Fatalf("jj clone: %v", err)
	}
	return dest
}

// jjInit creates a remoteless colocated jj repo at dest.
func (f *fixture) jjInit(dest string) string {
	f.t.Helper()
	cmd := exec.Command("jj", "git", "init", "--colocate", dest)
	cmd.Dir = f.root
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("jj git init --colocate %s: %v: %s", dest, err, out)
	}
	return dest
}

// advanceOrigin appends content to README via the seed clone and pushes a new
// commit to origin, returning the new origin main commit hash.
func (f *fixture) advanceOrigin(content string) string {
	f.t.Helper()
	cur := f.readFile(f.seed, "README.md")
	f.writeFile(f.seed, "README.md", cur+content+"\n")
	f.runGit(f.seed, "commit", "-aqm", content)
	f.runGit(f.seed, "push", "-q", "origin", "main")
	return f.originMain()
}

// seedGenerated writes a .gitattributes marking *.gen as linguist-generated plus
// an initial build.gen, commits, and pushes both onto trunk via the seed clone.
func (f *fixture) seedGenerated() {
	f.t.Helper()
	f.writeFile(f.seed, ".gitattributes", "*.gen linguist-generated\n")
	f.writeFile(f.seed, "build.gen", "generated v1\n")
	f.runGit(f.seed, "add", ".gitattributes", "build.gen")
	f.runGit(f.seed, "commit", "-qm", "seed generated")
	f.runGit(f.seed, "push", "-q", "origin", "main")
}

// advanceOriginPath writes content to path via the seed clone and pushes a new
// commit to origin, returning the new origin main commit hash.
func (f *fixture) advanceOriginPath(path, content string) string {
	f.t.Helper()
	f.writeFile(f.seed, path, content)
	f.runGit(f.seed, "add", path)
	f.runGit(f.seed, "commit", "-qm", "advance "+path)
	f.runGit(f.seed, "push", "-q", "origin", "main")
	return f.originMain()
}

// originMain returns the bare origin's current main commit hash.
func (f *fixture) originMain() string {
	f.t.Helper()
	return strings.TrimSpace(f.runGit(f.root, "-C", f.origin, "rev-parse", "main"))
}

// snapshotJJ forces a jj snapshot of the working copy (without --ignore-working-copy),
// mimicking the real user activity that records edits the poller later observes.
func (f *fixture) snapshotJJ(repo string) {
	f.t.Helper()
	f.runJJ(repo, "status")
}

func (f *fixture) configGit(dir string) {
	f.t.Helper()
	f.runGit(dir, "config", "user.name", "Test User")
	f.runGit(dir, "config", "user.email", "test@example.com")
}

func (f *fixture) runGit(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (f *fixture) runJJ(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("jj", append([]string{"--repository", dir}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("jj %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// runJJConflicts returns the conflict list from `jj resolve --list`, treating the
// no-conflicts case (which exits non-zero with "No conflicts found" on stderr) as
// an empty list rather than a fatal error.
func (f *fixture) runJJConflicts(dir string) string {
	f.t.Helper()
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

func (f *fixture) writeFile(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

func (f *fixture) readFile(dir, name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		f.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func (f *fixture) fileExists(dir, name string) bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}
