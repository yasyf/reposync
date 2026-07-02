package vcs

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
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
