package vcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// opTimeout is a hard backstop on any single git/jj invocation so a wedged
// network operation can never live unbounded; a tighter caller deadline wins.
const opTimeout = 5 * time.Minute

// termGrace is how long a canceled git/jj process group gets to unwind after
// SIGTERM — abort its ref transaction and unlink its lock files — before Go's
// SIGKILL backstop, and how long Wait waits for an orphaned grandchild to release
// the output pipes before force-closing them.
const termGrace = 10 * time.Second

// gitSSHCommand makes git/jj fail fast on a dead SSH connection: BatchMode
// prevents credential prompts, ConnectTimeout caps the handshake, and the
// ServerAlive probes tear down a silently-dropped connection in ~15s. It is
// additive — ~/.ssh/config Host blocks still apply.
const gitSSHCommand = "ssh -o BatchMode=yes -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3"

// gitConfigEnv pins command-scope git config for every reposync-driven git —
// direct or spawned by jj — so no invocation ever runs a synchronous gc/
// pack-refs inside a killable window. reposync is a syncer, not a maintainer:
// repo gc stays with the user's own git usage.
var gitConfigEnv = configEnv(nil)

var baseGitConfig = [][2]string{
	{"gc.auto", "0"},
	{"maintenance.auto", "false"},
}

var readOnlyGitConfig = [][2]string{
	{"core.fsmonitor", "false"},
	{"core.hooksPath", os.DevNull},
	{"core.splitIndex", "false"},
}

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

// Cmd is one git or jj invocation for Exec. Env is appended after the
// reposync defaults, so a later GIT_CONFIG_COUNT (from ReadOnlyGitEnv or
// FilterOverrideEnv) replaces the default one. Stdin and Stdout stream; stderr
// is captured into the returned error.
type Cmd struct {
	Dir    string
	Name   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
}

// Exec runs c in its own process group under the per-invocation timeout: a
// canceled context sends SIGTERM to the whole group before Go's SIGKILL
// backstop. A failure carries the exit code and trimmed stderr.
func Exec(ctx context.Context, c Cmd) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	//nolint:gosec // G204: reposync drives git/jj by design; name and args come from trusted repo config and internal call sites, not untrusted input.
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = termGrace
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCommand)
	cmd.Env = append(cmd.Env, gitConfigEnv...)
	cmd.Env = append(cmd.Env, c.Env...)
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return &cmdError{name: c.Name, args: c.Args, code: code, stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	return nil
}

// ReadOnlyGitEnv is the environment for git reads against a repository
// reposync must never write: no optional index refresh (GIT_OPTIONAL_LOCKS=0),
// no hook or fsmonitor callback (core.hooksPath and core.fsmonitor=false, which
// a submodule's child git inherits), no shared split-index file written beside
// a private index, no lazy fetch of an object a partial clone lacks
// (GIT_NO_LAZY_FETCH=1), no credential prompt, and the default gc/maintenance
// suppression.
func ReadOnlyGitEnv() []string {
	return readOnlyEnv(nil)
}

// FilterOverrideEnv is ReadOnlyGitEnv plus command-scope config that blanks
// the clean and process command of every filter driver configured for any
// repository in dirs and marks each driver not required, so a status read
// hashes raw worktree bytes and never runs a filter that writes (git-lfs's
// clean filter writes <common>/lfs/objects). A status that recurses into
// submodules runs their child gits under this same config, so dirs must name
// every submodule the read visits.
func FilterOverrideEnv(ctx context.Context, dirs ...string) ([]string, error) {
	seen := map[string]bool{}
	var pairs [][2]string
	for _, dir := range dirs {
		var out bytes.Buffer
		err := Exec(ctx, Cmd{
			Dir:    dir,
			Name:   "git",
			Args:   []string{"-C", dir, "config", "-z", "--get-regexp", `^filter\..*\.(clean|process)$`},
			Env:    ReadOnlyGitEnv(),
			Stdout: &out,
		})
		if err != nil && exitCode(err) != 1 {
			return nil, fmt.Errorf("list filter drivers in %s: %w", dir, err)
		}
		drivers, err := filterDrivers(out.Bytes())
		if err != nil {
			return nil, err
		}
		for _, d := range drivers {
			if seen[d] {
				continue
			}
			seen[d] = true
			pairs = append(
				pairs,
				[2]string{"filter." + d + ".clean", ""},
				[2]string{"filter." + d + ".process", ""},
				[2]string{"filter." + d + ".required", "false"},
			)
		}
	}
	return readOnlyEnv(pairs), nil
}

func readOnlyEnv(extra [][2]string) []string {
	return append(configEnv(slices.Concat(readOnlyGitConfig, extra)), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
}

func filterDrivers(out []byte) ([]string, error) {
	var drivers []string
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		key, _, _ := bytes.Cut(rec, []byte{'\n'})
		name, ok := strings.CutPrefix(string(key), "filter.")
		if !ok {
			return nil, fmt.Errorf("unexpected filter config key %q", key)
		}
		dot := strings.LastIndexByte(name, '.')
		if dot <= 0 {
			return nil, fmt.Errorf("unexpected filter config key %q", key)
		}
		drivers = append(drivers, name[:dot])
	}
	return drivers, nil
}

func configEnv(extra [][2]string) []string {
	pairs := append(slices.Clone(baseGitConfig), extra...)
	env := make([]string, 0, 1+2*len(pairs))
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(pairs)))
	for i, p := range pairs {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runStdin(ctx, dir, "", name, args...)
}

func runStdin(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	var stdout bytes.Buffer
	c := Cmd{Dir: dir, Name: name, Args: args, Stdout: &stdout}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	err := Exec(ctx, c)
	return stdout.String(), err
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
