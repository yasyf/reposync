package hostregistry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tailscaleSelfJSON = `{"Self":{"DNSName":"yasyf.tail71af5d.ts.net.","HostName":"yBook Pro"}}`

func TestDetectSelf(t *testing.T) {
	r := NewMockRunner().
		OnLocal("tailscale status --json", tailscaleSelfJSON, nil).
		OnLocal("id -un", "yasyf\n", nil)

	self, err := DetectSelf(context.Background(), r)
	if err != nil {
		t.Fatalf("DetectSelf: %v", err)
	}
	if self != "yasyf@yasyf" {
		t.Fatalf("self = %q, want %q", self, "yasyf@yasyf")
	}
}

func TestDetectSelfTailscaleError(t *testing.T) {
	r := NewMockRunner().
		OnLocal("tailscale status --json", "", errors.New("exec: tailscale: not found"))

	_, err := DetectSelf(context.Background(), r)
	if err == nil {
		t.Fatal("expected error when tailscale is absent")
	}
	if !strings.Contains(err.Error(), "--self") {
		t.Fatalf("error %q should mention --self override", err)
	}
}

func TestVerify(t *testing.T) {
	cases := []struct {
		name             string
		runner           *MockRunner
		wantReachable    bool
		wantBootstrapped bool
		wantVersion      string
		wantErr          bool
	}{
		{
			name: "bootstrapped",
			runner: NewMockRunner().
				OnSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
				OnSSH("reposync --version", "reposync 1.2.3\n", nil),
			wantReachable:    true,
			wantBootstrapped: true,
			wantVersion:      "reposync 1.2.3",
		},
		{
			name: "reachable but not installed",
			runner: NewMockRunner().
				OnSSH("command -v reposync", "", errors.New("exit status 1")).
				OnSSH("true", "", nil),
			wantReachable:    true,
			wantBootstrapped: false,
		},
		{
			name: "unreachable",
			runner: NewMockRunner().
				OnSSH("command -v reposync", "", errors.New("exit status 1")).
				OnSSH("true", "", errors.New("connection refused")),
			wantReachable:    false,
			wantBootstrapped: false,
			wantErr:          true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Verify(context.Background(), tc.runner, "yasyf@host")
			if res.Target != "yasyf@host" {
				t.Fatalf("Target = %q, want %q", res.Target, "yasyf@host")
			}
			if res.Reachable != tc.wantReachable {
				t.Fatalf("Reachable = %v, want %v", res.Reachable, tc.wantReachable)
			}
			if res.Bootstrapped != tc.wantBootstrapped {
				t.Fatalf("Bootstrapped = %v, want %v", res.Bootstrapped, tc.wantBootstrapped)
			}
			if res.Version != tc.wantVersion {
				t.Fatalf("Version = %q, want %q", res.Version, tc.wantVersion)
			}
			if tc.wantErr && res.Err == nil {
				t.Fatal("expected Err to be set")
			}
			if !tc.wantErr && res.Err != nil {
				t.Fatalf("unexpected Err: %v", res.Err)
			}
		})
	}
}

func TestVerifyVersionProbeError(t *testing.T) {
	r := NewMockRunner().
		OnSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
		OnSSH("reposync --version", "", errors.New("exit status 2"))

	res := Verify(context.Background(), r, "yasyf@host")
	if !res.Reachable || !res.Bootstrapped {
		t.Fatalf("install succeeded, want reachable+bootstrapped, got %+v", res)
	}
	if res.Version != "" {
		t.Fatalf("Version = %q, want empty on probe error", res.Version)
	}
	if res.Err != nil {
		t.Fatalf("version probe error must not surface: %v", res.Err)
	}
}

func TestVerifyAll(t *testing.T) {
	hosts := []string{"up@one", "down@two", "up@three"}

	r := NewMockRunner().
		OnSSH("command -v reposync", "/opt/homebrew/bin/reposync\n", nil).
		OnSSH("reposync --version", "reposync 1.0.0\n", nil)
	wrapped := &targetFailingRunner{Runner: r, failTarget: "down@two"}

	got := VerifyAll(context.Background(), wrapped, hosts)
	if len(got) != len(hosts) {
		t.Fatalf("got %d results, want %d", len(got), len(hosts))
	}
	for i, h := range hosts {
		if got[i].Target != h {
			t.Fatalf("result[%d].Target = %q, want %q (order not preserved)", i, got[i].Target, h)
		}
	}
	if !got[0].Reachable || !got[0].Bootstrapped {
		t.Fatalf("up@one should be green, got %+v", got[0])
	}
	if !got[2].Reachable || !got[2].Bootstrapped {
		t.Fatalf("up@three should be green, got %+v", got[2])
	}
	if got[1].Reachable || got[1].Bootstrapped || got[1].Err == nil {
		t.Fatalf("down@two should be red with Err set, got %+v", got[1])
	}
}

func TestRemoveHost(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := Update(context.Background(), func(g *Registry) error {
		g.UpsertHost("a@host")
		g.UpsertHost("b@host")
		return nil
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	if err := RemoveHost(context.Background(), "a@host"); err != nil {
		t.Fatalf("RemoveHost: %v", err)
	}

	persisted, err := Load()
	if err != nil {
		t.Fatalf("load persisted registry: %v", err)
	}
	if contains(persisted.Hosts, "a@host") {
		t.Fatalf("host not removed: %v", persisted.Hosts)
	}
	if !contains(persisted.Hosts, "b@host") {
		t.Fatalf("unrelated host dropped: %v", persisted.Hosts)
	}
}

// TestUpdatePreservesForeignKeys is the sharpest invariant: a hostregistry.Update
// that mutates only self/hosts must leave reposync's repo-specific keys (repos,
// settings, default_location) byte-for-byte intact in state.json.
func TestUpdatePreservesForeignKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg := filepath.Join(dir, configSubdir)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}

	// A full reposync state.json, including a repos array and settings, written by
	// a reposync writer that hostregistry knows nothing about.
	original := `{
  "default_location": "~/Code",
  "self": "yasyf@old",
  "hosts": [],
  "repos": [
    {"relpath": "cc-review", "origin": "https://github.com/yasyf/cc-review.git", "trunk": "main", "local_only": false},
    {"relpath": "scratch", "origin": "", "trunk": "", "local_only": true}
  ],
  "settings": {"interval": "15m0s", "idle_threshold": "5m0s", "watch_debounce": "3s", "repo_op_timeout": "2m0s", "push_after": "24h0m0s"}
}`
	statePath := filepath.Join(cfg, stateFile)
	if err := os.WriteFile(statePath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// Capture the repos + settings sub-objects before the host upsert.
	var before map[string]json.RawMessage
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}

	if _, err := Update(context.Background(), func(g *Registry) error {
		g.UpsertHost("yasyf@yasyf-home")
		g.Self = "yasyf@new"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	//nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("re-parse state: %v", err)
	}

	// repos and settings must be unchanged; the host upsert touched only self+hosts.
	for _, key := range []string{"repos", "settings", "default_location"} {
		if !bytesEqualJSON(t, before[key], after[key]) {
			t.Fatalf("%s changed across host upsert:\n before: %s\n  after: %s", key, before[key], after[key])
		}
	}

	// And the host-identity keys did update.
	g, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.Self != "yasyf@new" {
		t.Fatalf("Self = %q, want yasyf@new", g.Self)
	}
	if !contains(g.Hosts, "yasyf@yasyf-home") {
		t.Fatalf("Hosts = %v, want to contain yasyf@yasyf-home", g.Hosts)
	}
}

// bytesEqualJSON compares two raw JSON values for semantic equality (re-encoding
// to normalize whitespace), so a reformat that preserves content still passes.
func bytesEqualJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a (%s): %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b (%s): %v", b, err)
	}
	an, err := json.Marshal(av)
	if err != nil {
		t.Fatal(err)
	}
	bn, err := json.Marshal(bv)
	if err != nil {
		t.Fatal(err)
	}
	return string(an) == string(bn)
}

// targetFailingRunner wraps a Runner and forces SSH to one target to fail.
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

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
