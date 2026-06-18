package host

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/reposync/internal/state"
)

// call is one recorded invocation against the fake Runner.
type call struct {
	kind   string // "local" or "ssh"
	target string // ssh target, or "" for local
	cmd    string // ssh remote command, or "name arg arg" for local
}

// reply is a scripted result returned for a matching call.
type reply struct {
	out string
	err error
}

// fakeRunner records every Local/SSH call in order and returns scripted replies.
// Local replies key on the joined "name args"; SSH replies key on the remote
// command substring so a test can script by intent (e.g. "command -v reposync").
type fakeRunner struct {
	mu       sync.Mutex
	calls    []call
	localOn  map[string]reply
	sshOn    []sshRule
	sshDef   reply
	hasSSHDe bool
}

type sshRule struct {
	contains string
	reply    reply
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{localOn: map[string]reply{}}
}

func (f *fakeRunner) onLocal(key string, out string, err error) *fakeRunner {
	f.localOn[key] = reply{out: out, err: err}
	return f
}

func (f *fakeRunner) onSSH(contains, out string, err error) *fakeRunner {
	f.sshOn = append(f.sshOn, sshRule{contains: contains, reply: reply{out: out, err: err}})
	return f
}

func (f *fakeRunner) defaultSSH(out string, err error) *fakeRunner {
	f.sshDef = reply{out: out, err: err}
	f.hasSSHDe = true
	return f
}

func (f *fakeRunner) Local(_ context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, call{kind: "local", cmd: key})
	r, ok := f.localOn[key]
	f.mu.Unlock()
	if !ok {
		return "", errors.New("unscripted local: " + key)
	}
	return r.out, r.err
}

func (f *fakeRunner) SSH(_ context.Context, target, remoteCmd string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{kind: "ssh", target: target, cmd: remoteCmd})
	f.mu.Unlock()
	for _, rule := range f.sshOn {
		if strings.Contains(remoteCmd, rule.contains) {
			return rule.reply.out, rule.reply.err
		}
	}
	if f.hasSSHDe {
		return f.sshDef.out, f.sshDef.err
	}
	return "", errors.New("unscripted ssh: " + remoteCmd)
}

func (f *fakeRunner) sshCmds(target string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.kind == "ssh" && c.target == target {
			out = append(out, c.cmd)
		}
	}
	return out
}

func (f *fakeRunner) sshCmdsAll() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.kind == "ssh" {
			out = append(out, c.cmd)
		}
	}
	return out
}

func emptyState(t *testing.T) *state.State {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

const tailscaleJSON = `{"Self":{"DNSName":"yasyf.tail71af5d.ts.net.","HostName":"yBook Pro"}}`

func TestDetectSelf(t *testing.T) {
	r := newFakeRunner().
		onLocal("tailscale status --json", tailscaleJSON, nil).
		onLocal("id -un", "yasyf\n", nil)

	self, err := DetectSelf(context.Background(), r)
	if err != nil {
		t.Fatalf("DetectSelf: %v", err)
	}
	if self != "yasyf@yasyf" {
		t.Fatalf("self = %q, want %q", self, "yasyf@yasyf")
	}
}

func TestDetectSelfTailscaleError(t *testing.T) {
	r := newFakeRunner().
		onLocal("tailscale status --json", "", errors.New("exec: tailscale: not found"))

	_, err := DetectSelf(context.Background(), r)
	if err == nil {
		t.Fatal("expected error when tailscale is absent")
	}
	if !strings.Contains(err.Error(), "--self") {
		t.Fatalf("error %q should mention --self override", err)
	}
}

func TestAddHostForwardNotInstalled(t *testing.T) {
	st := emptyState(t)
	st.Repos = []state.Repo{
		{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main", LocalOnly: false},
		{Relpath: "notes", Origin: "", Trunk: "main", LocalOnly: true},
	}

	r := newFakeRunner().
		onSSH("command -v reposync", "", errors.New("exit status 1")).
		defaultSSH("", nil)

	_, err := AddHost(context.Background(), st, r, "yasyf@yasyf-home", "yasyf@yasyf", false)
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	got := r.sshCmds("yasyf@yasyf-home")
	want := []string{
		"command -v reposync",
		"brew tap yasyf/tap && brew trust yasyf/tap && brew install --cask yasyf/tap/reposync",
		"reposync host add yasyf@yasyf --no-recurse",
		"reposync repo add-remote --origin 'https://github.com/yasyf/cc-review.git' --relpath 'cc-review' --trunk 'main'",
		"reposync reconcile",
		"reposync install",
	}
	assertSeq(t, got, want)

	// loop guard: the inverse registration always carries --no-recurse.
	if !strings.Contains(got[2], "--no-recurse") {
		t.Fatalf("inverse host add %q must contain --no-recurse", got[2])
	}
	// local_only repo must NOT be propagated.
	for _, c := range got {
		if strings.Contains(c, "notes") {
			t.Fatalf("local_only repo was propagated: %q", c)
		}
	}
	// exactly one add-remote (only the non-local-only repo).
	if n := countContains(got, "add-remote"); n != 1 {
		t.Fatalf("got %d add-remote calls, want 1", n)
	}
}

func TestAddHostForwardAlreadyInstalled(t *testing.T) {
	st := emptyState(t)
	st.Repos = []state.Repo{
		{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"},
	}

	r := newFakeRunner().
		onSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
		defaultSSH("", nil)

	_, err := AddHost(context.Background(), st, r, "yasyf@yasyf-home", "yasyf@yasyf", false)
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	got := r.sshCmds("yasyf@yasyf-home")
	want := []string{
		"command -v reposync",
		"reposync host add yasyf@yasyf --no-recurse",
		"reposync repo add-remote --origin 'https://github.com/yasyf/cc-review.git' --relpath 'cc-review' --trunk 'main'",
		"reposync reconcile",
		"reposync install",
	}
	assertSeq(t, got, want)

	for _, c := range got {
		if strings.Contains(c, "brew install") {
			t.Fatalf("brew install should be skipped when already installed, saw %q", c)
		}
	}
}

func TestAddHostNoRecurse(t *testing.T) {
	st := emptyState(t)
	st.Repos = []state.Repo{
		{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"},
	}

	r := newFakeRunner() // no SSH scripted: any ssh call would error/record.

	_, err := AddHost(context.Background(), st, r, "yasyf@yasyf", "yasyf@yasyf-home", true)
	if err != nil {
		t.Fatalf("AddHost no-recurse: %v", err)
	}

	if cmds := r.sshCmdsAll(); len(cmds) != 0 {
		t.Fatalf("no-recurse must make zero ssh calls, got %v", cmds)
	}
	persisted, err := state.Load()
	if err != nil {
		t.Fatalf("load persisted state: %v", err)
	}
	if !contains(persisted.Hosts, "yasyf@yasyf") {
		t.Fatalf("host not registered in persisted state: %v", persisted.Hosts)
	}
}

func TestAddHostIdempotent(t *testing.T) {
	st := emptyState(t)
	r := newFakeRunner()

	for i := 0; i < 2; i++ {
		if _, err := AddHost(context.Background(), st, r, "yasyf@yasyf", "yasyf@yasyf-home", true); err != nil {
			t.Fatalf("AddHost iteration %d: %v", i, err)
		}
	}
	persisted, err := state.Load()
	if err != nil {
		t.Fatalf("load persisted state: %v", err)
	}
	if n := countEqual(persisted.Hosts, "yasyf@yasyf"); n != 1 {
		t.Fatalf("host duplicated: %v (count %d)", persisted.Hosts, n)
	}
}

func TestAddHostBrewNoCask(t *testing.T) {
	st := emptyState(t)
	r := newFakeRunner().
		onSSH("command -v reposync", "", errors.New("exit status 1")).
		onSSH("brew install", "Error: No available formula or cask with the name \"yasyf/tap/reposync\".", errors.New("exit status 1"))

	_, err := AddHost(context.Background(), st, r, "yasyf@yasyf-home", "yasyf@yasyf", false)
	if err == nil {
		t.Fatal("expected error when the cask is unpublished")
	}
	if !strings.Contains(err.Error(), "release") {
		t.Fatalf("error %q should point at publishing a release", err)
	}
}

func TestPropagateRepo(t *testing.T) {
	st := emptyState(t)
	st.Hosts = []string{"yasyf@yasyf-home", "yasyf@yasyf-laptop"}

	r := newFakeRunner().defaultSSH("", nil)

	repo := state.Repo{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"}
	if err := PropagateRepo(context.Background(), st, r, repo); err != nil {
		t.Fatalf("PropagateRepo: %v", err)
	}

	for _, h := range st.Hosts {
		cmds := r.sshCmds(h)
		if len(cmds) != 1 || !strings.Contains(cmds[0], "add-remote") {
			t.Fatalf("host %s got %v, want a single add-remote", h, cmds)
		}
	}
}

func TestPropagateRepoSkipsLocalOnly(t *testing.T) {
	st := emptyState(t)
	st.Hosts = []string{"yasyf@yasyf-home"}
	r := newFakeRunner().defaultSSH("", nil)

	cases := []state.Repo{
		{Relpath: "notes", Origin: "", Trunk: "main", LocalOnly: true},
		{Relpath: "scratch", Origin: "https://github.com/yasyf/scratch.git", Trunk: "main", LocalOnly: true},
	}
	for _, repo := range cases {
		if err := PropagateRepo(context.Background(), st, r, repo); err != nil {
			t.Fatalf("PropagateRepo %s: %v", repo.Relpath, err)
		}
	}
	if cmds := r.sshCmdsAll(); len(cmds) != 0 {
		t.Fatalf("local_only/remoteless repos must not propagate, got %v", cmds)
	}
}

func TestRemoteReconcileDownHostContinues(t *testing.T) {
	st := emptyState(t)
	st.Hosts = []string{"up@host", "down@host"}

	r := newFakeRunner().
		onSSH("reposync reconcile", "", nil)
	// scripted by substring match above returns nil for both; override down host
	// by routing through a wrapper that fails one target.
	wrapped := &targetFailingRunner{Runner: r, failTarget: "down@host"}

	err := RemoteReconcile(context.Background(), st, wrapped)
	if err == nil {
		t.Fatal("expected an aggregated error when a host is down")
	}
	if !strings.Contains(err.Error(), "down@host") {
		t.Fatalf("error %q should name the down host", err)
	}
	// the up host was still reconciled (not aborted by the down one).
	if cmds := r.sshCmds("up@host"); len(cmds) != 1 {
		t.Fatalf("up host should have been reconciled, got %v", cmds)
	}
}

func TestRemoveHost(t *testing.T) {
	st := emptyState(t)
	st.Hosts = []string{"a@host", "b@host"}
	if err := st.Save(); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if err := RemoveHost("a@host"); err != nil {
		t.Fatalf("RemoveHost: %v", err)
	}

	persisted, err := state.Load()
	if err != nil {
		t.Fatalf("load persisted state: %v", err)
	}
	if contains(persisted.Hosts, "a@host") {
		t.Fatalf("host not removed: %v", persisted.Hosts)
	}
	if !contains(persisted.Hosts, "b@host") {
		t.Fatalf("unrelated host dropped: %v", persisted.Hosts)
	}
}

// targetFailingRunner wraps a Runner and and forces SSH to one target to fail,
// exercising the down-host-continues path without ordering assumptions.
type targetFailingRunner struct {
	Runner
	failTarget string
}

func (w *targetFailingRunner) SSH(ctx context.Context, target, remoteCmd string) (string, error) {
	out, err := w.Runner.SSH(ctx, target, remoteCmd)
	if target == w.failTarget {
		return out, errors.New("connection refused")
	}
	return out, err
}

func assertSeq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q\n full got: %v", i, got[i], want[i], got)
		}
	}
}

func countContains(s []string, sub string) int {
	n := 0
	for _, v := range s {
		if strings.Contains(v, sub) {
			n++
		}
	}
	return n
}

func countEqual(s []string, want string) int {
	n := 0
	for _, v := range s {
		if v == want {
			n++
		}
	}
	return n
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
