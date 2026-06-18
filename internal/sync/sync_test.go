package sync

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/config"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func clone(t *testing.T, bare, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	run(t, t.TempDir(), "clone", bare, dir)
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	run(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func commit(t *testing.T, dir, file, body string) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := exec.Command("touch", path).Run(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-m", body)
}

// setup creates a bare remote with an initial commit on main and returns the
// bare path plus a fresh clone tracking origin/main.
func setup(t *testing.T) (bare, repo string) {
	t.Helper()
	bare = filepath.Join(t.TempDir(), "remote.git")
	run(t, t.TempDir(), "init", "--bare", "-b", "main", bare)

	seed := clone(t, bare, "seed")
	run(t, seed, "checkout", "-b", "main")
	commit(t, seed, "README", "initial")
	run(t, seed, "push", "-u", "origin", "main")

	return bare, clone(t, bare, "repo")
}

func syncOnce(t *testing.T, path string, auto bool) Result {
	t.Helper()
	repo := config.Repo{Path: path, Remote: "origin", Branch: "main", AutoCommit: auto}
	return syncRepo(context.Background(), repo, git)
}

func TestSyncUpToDate(t *testing.T) {
	_, repo := setup(t)

	res := syncOnce(t, repo, false)
	if res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}
	if res.State != StateUpToDate {
		t.Errorf("state = %s, want up-to-date", res.State)
	}
}

func TestSyncPushesAhead(t *testing.T) {
	bare, repo := setup(t)
	commit(t, repo, "local.txt", "local change")

	res := syncOnce(t, repo, false)
	if res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}
	if res.State != StatePushed {
		t.Errorf("state = %s, want pushed", res.State)
	}

	// The new commit must now be reachable in the bare remote.
	verify := clone(t, bare, "verify")
	out := run(t, verify, "log", "--oneline")
	if !strings.Contains(out, "local change") {
		t.Errorf("remote missing pushed commit; log:\n%s", out)
	}
}

func TestSyncPullsBehind(t *testing.T) {
	bare, repo := setup(t)

	publisher := clone(t, bare, "publisher")
	commit(t, publisher, "upstream.txt", "upstream change")
	run(t, publisher, "push", "origin", "main")

	res := syncOnce(t, repo, false)
	if res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}
	if res.State != StatePulled {
		t.Errorf("state = %s, want pulled", res.State)
	}
	out := run(t, repo, "log", "--oneline")
	if !strings.Contains(out, "upstream change") {
		t.Errorf("local missing pulled commit; log:\n%s", out)
	}
}

func TestSyncDivergedFails(t *testing.T) {
	bare, repo := setup(t)

	publisher := clone(t, bare, "publisher")
	commit(t, publisher, "upstream.txt", "upstream change")
	run(t, publisher, "push", "origin", "main")

	commit(t, repo, "local.txt", "local change")

	res := syncOnce(t, repo, false)
	if res.State != StateDiverged {
		t.Errorf("state = %s, want diverged", res.State)
	}
	if res.Err == nil {
		t.Error("expected diverged sync to report an error")
	}
}

func TestSyncAutoCommit(t *testing.T) {
	_, repo := setup(t)
	// Dirty working tree, no commit yet.
	if err := exec.Command("touch", filepath.Join(repo, "dirty.txt")).Run(); err != nil {
		t.Fatalf("touch: %v", err)
	}

	res := syncOnce(t, repo, true)
	if res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}
	if res.State != StatePushed {
		t.Errorf("state = %s, want pushed", res.State)
	}
	if status := run(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("working tree still dirty: %q", status)
	}
}

func TestSyncNonRepoFails(t *testing.T) {
	res := syncOnce(t, t.TempDir(), false)
	if res.Err == nil {
		t.Error("expected error syncing a non-git directory")
	}
}
