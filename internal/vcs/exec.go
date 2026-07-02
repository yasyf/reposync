package vcs

import (
	"bytes"
	"context"
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
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
