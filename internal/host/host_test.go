package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/synckit/hostregistry"
)

func emptyState(t *testing.T) *state.State {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

func TestAddHostForwardNotInstalled(t *testing.T) {
	emptyState(t)

	r := hostregistry.NewMockRunner().
		OnSSH("command -v reposync", "", errors.New("exit status 1")).
		DefaultSSH("", nil)

	_, err := AddHost(context.Background(), r, "yasyf@yasyf-home", "yasyf@yasyf", false)
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	got := r.SSHCmds("yasyf@yasyf-home")
	want := []string{
		"command -v reposync",
		"brew tap yasyf/tap && brew trust yasyf/tap && brew install --cask yasyf/tap/reposync",
		"reposync host add yasyf@yasyf --no-recurse",
		"reposync reconcile",
		"reposync install",
	}
	assertSeq(t, got, want)

	// loop guard: the inverse registration always carries --no-recurse.
	if !strings.Contains(got[2], "--no-recurse") {
		t.Fatalf("inverse host add %q must contain --no-recurse", got[2])
	}
	// No repo push: convergence pulls shared repos on the peer's reconcile.
	if n := countContains(got, "add-remote"); n != 0 {
		t.Fatalf("got %d add-remote calls, want 0 (pull-merge, no push)", n)
	}
}

func TestAddHostForwardAlreadyInstalled(t *testing.T) {
	emptyState(t)

	r := hostregistry.NewMockRunner().
		OnSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
		DefaultSSH("", nil)

	_, err := AddHost(context.Background(), r, "yasyf@yasyf-home", "yasyf@yasyf", false)
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	got := r.SSHCmds("yasyf@yasyf-home")
	want := []string{
		"command -v reposync",
		"reposync host add yasyf@yasyf --no-recurse",
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
	emptyState(t)

	r := hostregistry.NewMockRunner() // no SSH scripted: any ssh call would error/record.

	_, err := AddHost(context.Background(), r, "yasyf@yasyf", "yasyf@yasyf-home", true)
	if err != nil {
		t.Fatalf("AddHost no-recurse: %v", err)
	}

	if cmds := r.SSHCmdsAll(); len(cmds) != 0 {
		t.Fatalf("no-recurse must make zero ssh calls, got %v", cmds)
	}
	reg, err := state.Config.Load()
	if err != nil {
		t.Fatalf("load persisted registry: %v", err)
	}
	if !contains(reg.Hosts, "yasyf@yasyf") {
		t.Fatalf("host not registered in persisted registry: %v", reg.Hosts)
	}
	if reg.Self != "yasyf@yasyf-home" {
		t.Fatalf("self not persisted: got %q, want %q", reg.Self, "yasyf@yasyf-home")
	}
}

func TestAddHostDetectsAndPersistsSelf(t *testing.T) {
	emptyState(t)

	r := hostregistry.NewMockRunner().
		OnLocal("tailscale status --json", tailscaleJSON, nil).
		OnLocal("id -un", "yasyf\n", nil).
		OnSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
		DefaultSSH("", nil)

	// self == "" forces tailscale auto-detection, resolving to yasyf@yasyf.
	if _, err := AddHost(context.Background(), r, "yasyf@yasyf-home", "", false); err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	got := r.SSHCmds("yasyf@yasyf-home")
	if countContains(got, "reposync host add yasyf@yasyf --no-recurse") != 1 {
		t.Fatalf("inverse registration must carry the detected self, got %v", got)
	}

	reg, err := state.Config.Load()
	if err != nil {
		t.Fatalf("load persisted registry: %v", err)
	}
	if reg.Self != "yasyf@yasyf" {
		t.Fatalf("detected self not persisted: got %q, want %q", reg.Self, "yasyf@yasyf")
	}
}

func TestAddHostIdempotent(t *testing.T) {
	emptyState(t)
	r := hostregistry.NewMockRunner()

	for i := 0; i < 2; i++ {
		if _, err := AddHost(context.Background(), r, "yasyf@yasyf", "yasyf@yasyf-home", true); err != nil {
			t.Fatalf("AddHost iteration %d: %v", i, err)
		}
	}
	reg, err := state.Config.Load()
	if err != nil {
		t.Fatalf("load persisted registry: %v", err)
	}
	if n := countEqual(reg.Hosts, "yasyf@yasyf"); n != 1 {
		t.Fatalf("host duplicated: %v (count %d)", reg.Hosts, n)
	}
}

func TestAddHostBrewNoCask(t *testing.T) {
	emptyState(t)
	r := hostregistry.NewMockRunner().
		OnSSH("command -v reposync", "", errors.New("exit status 1")).
		OnSSH("brew install", "Error: No available formula or cask with the name \"yasyf/tap/reposync\".", errors.New("exit status 1"))

	_, err := AddHost(context.Background(), r, "yasyf@yasyf-home", "yasyf@yasyf", false)
	if err == nil {
		t.Fatal("expected error when the cask is unpublished")
	}
	if !strings.Contains(err.Error(), "release") {
		t.Fatalf("error %q should point at publishing a release", err)
	}
}

func TestRemoveHost(t *testing.T) {
	emptyState(t)
	seedHosts(t, "a@host", "b@host")

	if err := RemoveHost(context.Background(), "a@host"); err != nil {
		t.Fatalf("RemoveHost: %v", err)
	}

	reg, err := state.Config.Load()
	if err != nil {
		t.Fatalf("load persisted registry: %v", err)
	}
	if contains(reg.Hosts, "a@host") {
		t.Fatalf("host not removed: %v", reg.Hosts)
	}
	if !contains(reg.Hosts, "b@host") {
		t.Fatalf("unrelated host dropped: %v", reg.Hosts)
	}
}

// seedHosts registers peers in the host registry under the temp config dir an
// emptyState/test has already set up, so RemoveHost — which reads the registry —
// sees them.
func seedHosts(t *testing.T, targets ...string) {
	t.Helper()
	if _, err := state.Config.Update(context.Background(), func(g *hostregistry.Registry) error {
		for _, target := range targets {
			g.UpsertHost(target)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed hosts: %v", err)
	}
}

func TestAddHostStreamEmitsEveryStep(t *testing.T) {
	emptyState(t)

	r := hostregistry.NewMockRunner().
		OnSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
		DefaultSSH("", nil)

	var streamed []string
	log, err := AddHostStream(context.Background(), r, "yasyf@yasyf-home", "yasyf@yasyf", false, func(msg string) {
		streamed = append(streamed, msg)
	})
	if err != nil {
		t.Fatalf("AddHostStream: %v", err)
	}
	assertSeq(t, streamed, log)
}

const tailscaleJSON = `{"Self":{"DNSName":"yasyf.tail71af5d.ts.net.","HostName":"yBook Pro"}}`

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
