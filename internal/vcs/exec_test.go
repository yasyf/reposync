package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunFailureTypedError pins run()'s failure contract: the cmdError keeps the
// pre-existing "<name> <args>: <cause>: <stderr>" message byte-for-byte, chains
// to the underlying *exec.ExitError, and classifies by exit code and captured
// stderr — never by the argv in the message.
func TestRunFailureTypedError(t *testing.T) {
	dir := t.TempDir()
	_, err := run(context.Background(), dir, "git", "-C", dir, "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("run git rev-parse in a non-repo dir succeeded, want failure")
	}
	wrapped := fmt.Errorf("resolve head: %w", err)

	var cerr *cmdError
	if !errors.As(wrapped, &cerr) {
		t.Fatalf("no cmdError in chain: %v", err)
	}
	if cerr.code != 128 {
		t.Errorf("code = %d, want 128", cerr.code)
	}
	if strings.TrimSpace(cerr.stderr) != cerr.stderr || cerr.stderr == "" {
		t.Errorf("stderr = %q, want non-empty and trimmed", cerr.stderr)
	}
	var exitErr *exec.ExitError
	if !errors.As(wrapped, &exitErr) {
		t.Errorf("no exec.ExitError in chain: %v", err)
	}
	want := fmt.Sprintf("git -C %s rev-parse HEAD: exit status 128: %s", dir, cerr.stderr)
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}

	if !stderrContains(wrapped, "not a git repository") {
		t.Errorf("stderrContains(not a git repository) = false, want true; stderr = %q", cerr.stderr)
	}
	if stderrContains(wrapped, "rev-parse") {
		t.Error("stderrContains matched argv \"rev-parse\", must match stderr only")
	}
	if got := exitCode(wrapped); got != 128 {
		t.Errorf("exitCode = %d, want 128", got)
	}
	if got := exitCode(errors.New("never ran a command")); got != -1 {
		t.Errorf("exitCode(non-command error) = %d, want -1", got)
	}
}

// TestRunCancelSendsSIGTERM proves a canceled invocation is signaled with SIGTERM,
// not SIGKILL: a child that traps TERM runs its cleanup handler (drops a sentinel)
// and exits well within termGrace, so a killed git/jj gets the chance to unwind its
// ref transaction and unlink its lock files.
func TestRunCancelSendsSIGTERM(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "cleaned")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// sleep runs in the background under `wait` so the trapped signal interrupts
	// promptly (a foreground child defers the trap until it exits); the trap kills
	// the child so no orphan keeps the output pipes open past WaitDelay.
	script := "trap 'touch " + sentinel + "; kill $p 2>/dev/null; exit 0' TERM; sleep 30 & p=$!; wait"
	start := time.Now()
	_, err := run(ctx, dir, "sh", "-c", script)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("run of a canceled command returned nil error")
	}
	if elapsed >= termGrace {
		t.Fatalf("run took %v, want well under termGrace %v (SIGKILL backstop fired)", elapsed, termGrace)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel missing: TERM handler never ran (SIGKILL?): %v", err)
	}
}

// TestRunSuppressesAutoMaintenance proves the gitConfigEnv plumbing reaches git end
// to end: reposync-driven git resolves gc.auto=0 and maintenance.auto=false from the
// command-scope config, so no invocation runs a synchronous gc/pack-refs.
func TestRunSuppressesAutoMaintenance(t *testing.T) {
	dir := t.TempDir()

	got, err := run(context.Background(), dir, "git", "config", "gc.auto")
	if err != nil {
		t.Fatalf("git config gc.auto: %v", err)
	}
	if strings.TrimSpace(got) != "0" {
		t.Fatalf("gc.auto = %q, want 0", strings.TrimSpace(got))
	}

	got, err = run(context.Background(), dir, "git", "config", "maintenance.auto")
	if err != nil {
		t.Fatalf("git config maintenance.auto: %v", err)
	}
	if strings.TrimSpace(got) != "false" {
		t.Fatalf("maintenance.auto = %q, want false", strings.TrimSpace(got))
	}
}
