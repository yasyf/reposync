package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestExecStreamsStdinAndStdout(t *testing.T) {
	var out strings.Builder
	err := Exec(context.Background(), Cmd{
		Dir:    t.TempDir(),
		Name:   "git",
		Args:   []string{"hash-object", "--stdin"},
		Stdin:  strings.NewReader("hello\n"),
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("hash-object = %q, want the blob id of hello", got)
	}
}

func TestReadOnlyGitEnv(t *testing.T) {
	env := ReadOnlyGitEnv()
	for _, want := range []string{
		"GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=gc.auto",
		"GIT_CONFIG_KEY_2=core.fsmonitor", "GIT_CONFIG_VALUE_2=false",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("ReadOnlyGitEnv() = %v, missing %q", env, want)
		}
	}
}

func TestFilterOverrideEnv(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "filter-ran")
	git := func(args ...string) string {
		t.Helper()
		out, err := run(context.Background(), dir, "git", append([]string{"-C", dir}, args...)...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	git("init", "-q")
	git("config", "user.name", "T")
	git("config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.dat filter=spy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.dat"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "init")
	git("config", "filter.spy.clean", "touch "+sentinel+"; cat")
	git("config", "filter.spy.required", "true")
	git("config", "filter.dotted.name.process", "false")

	other := t.TempDir()
	if _, err := run(context.Background(), other, "git", "-C", other, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"filter.spy.clean", "cat"}, {"filter.other.process", "false"}} {
		if _, err := run(context.Background(), other, "git", "-C", other, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}

	env, err := FilterOverrideEnv(context.Background(), dir, other)
	if err != nil {
		t.Fatalf("FilterOverrideEnv: %v", err)
	}
	keys := map[string]string{}
	for i := 0; ; i++ {
		k, ok := envValue(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		if !ok {
			break
		}
		v, _ := envValue(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		keys[k] = v
	}
	want := map[string]string{
		"gc.auto": "0", "maintenance.auto": "false", "core.fsmonitor": "false",
		"filter.spy.clean": "", "filter.spy.process": "", "filter.spy.required": "false",
		"filter.dotted.name.clean": "", "filter.dotted.name.process": "", "filter.dotted.name.required": "false",
		"filter.other.clean": "", "filter.other.process": "", "filter.other.required": "false",
	}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("override config = %v, want %v", keys, want)
	}
	if n, _ := envValue(env, "GIT_CONFIG_COUNT"); n != "12" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 12", n)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.dat"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err = Exec(context.Background(), Cmd{Dir: dir, Name: "git", Args: []string{"-C", dir, "status", "--porcelain"}, Env: env, Stdout: &out})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got := out.String(); got != " M a.dat\n" {
		t.Fatalf("status = %q, want a.dat modified", got)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean filter ran during status: stat sentinel = %v", err)
	}

	empty := t.TempDir()
	if _, err := run(context.Background(), empty, "git", "-C", empty, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	env, err = FilterOverrideEnv(context.Background(), empty)
	if err != nil {
		t.Fatalf("FilterOverrideEnv without drivers: %v", err)
	}
	if n, _ := envValue(env, "GIT_CONFIG_COUNT"); n != "3" {
		t.Fatalf("GIT_CONFIG_COUNT without drivers = %q, want 3", n)
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}
