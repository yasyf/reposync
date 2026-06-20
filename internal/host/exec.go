package host

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// sshOpTimeout is a hard backstop on any single ssh/local invocation so a wedged
// remote command can never live unbounded; a tighter caller deadline wins.
const sshOpTimeout = 60 * time.Second

// execRunner is the production Runner: Local shells out directly, SSH wraps the
// remote command so it sources brew's shellenv (non-interactive ssh on macOS
// lacks brew, and thus brew-installed reposync, on PATH).
type execRunner struct{}

// NewExecRunner returns the default Runner that executes commands locally and over ssh.
func NewExecRunner() Runner {
	return execRunner{}
}

func (execRunner) Local(ctx context.Context, name string, args ...string) (string, error) {
	return runCmd(ctx, name, args...)
}

func (execRunner) SSH(ctx context.Context, target, remoteCmd string) (string, error) {
	wrapped := fmt.Sprintf(`eval "$(/opt/homebrew/bin/brew shellenv)" && %s`, remoteCmd)
	return runCmd(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3", target, wrapped)
}

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sshOpTimeout)
	defer cancel()
	//nolint:gosec // G204: reposync is a CLI sync tool whose job is to run ssh/git; name and args come from trusted local state (registered hosts, repo config), not untrusted input.
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
