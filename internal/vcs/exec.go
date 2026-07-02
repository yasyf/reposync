package vcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// opTimeout is a hard backstop on any single git/jj invocation so a wedged
// network operation can never live unbounded; a tighter caller deadline wins.
const opTimeout = 5 * time.Minute

// gitSSHCommand makes git/jj fail fast on a dead SSH connection: BatchMode
// prevents credential prompts, ConnectTimeout caps the handshake, and the
// ServerAlive probes tear down a silently-dropped connection in ~15s. It is
// additive — ~/.ssh/config Host blocks still apply.
const gitSSHCommand = "ssh -o BatchMode=yes -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3"

// cmdError is a failed git/jj invocation, carrying the exit code and trimmed
// stderr so callers classify failures structurally instead of sniffing the
// argv-bearing message text.
type cmdError struct {
	name   string
	args   []string
	code   int
	stderr string
	err    error
}

func (e *cmdError) Error() string {
	return fmt.Sprintf("%s %s: %v: %s", e.name, strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *cmdError) Unwrap() error { return e.err }

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runStdin(ctx, dir, "", name, args...)
}

func runStdin(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	//nolint:gosec // G204: reposync drives git/jj by design; name and args come from trusted repo config and internal call sites, not untrusted input.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCommand)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return stdout.String(), &cmdError{name: name, args: args, code: code, stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	return stdout.String(), nil
}

// exitCode returns the exit code carried by the cmdError in err's chain, or -1
// when there is none (err is not a command failure, or the command never ran).
func exitCode(err error) int {
	var cerr *cmdError
	if !errors.As(err, &cerr) {
		return -1
	}
	return cerr.code
}

// stderrContains reports whether the stderr captured by the cmdError in err's
// chain contains sub, never matching the argv-bearing message text. An error
// that never ran a command has no captured stderr, so its plain text — which
// carries no argv — is matched instead.
func stderrContains(err error, sub string) bool {
	var cerr *cmdError
	if errors.As(err, &cerr) {
		return strings.Contains(cerr.stderr, sub)
	}
	return strings.Contains(err.Error(), sub)
}
