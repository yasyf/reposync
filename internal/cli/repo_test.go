package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestRepoLsShowsEnvColumn proves `repo ls` renders an ENV column reporting the
// per-repo env-sync setting: "on" by default, "off" for an opted-out repo.
func TestRepoLsShowsEnvColumn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	initializeProductState(t)
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.AddRepo(state.Repo{Relpath: "synced", Origin: "https://example.com/synced.git", Trunk: "main"})
		s.AddRepo(state.Repo{Relpath: "optout", Origin: "https://example.com/optout.git", Trunk: "main", NoEnvSync: true})
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	stdout, _, err := runCLI(t, "repo", "ls")
	if err != nil {
		t.Fatalf("repo ls: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 3 {
		t.Fatalf("repo ls output has too few rows:\n%s", stdout)
	}
	if !strings.Contains(lines[0], "ENV") {
		t.Fatalf("repo ls header = %q, want an ENV column", lines[0])
	}
	env := map[string]string{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		env[fields[0]] = fields[len(fields)-1]
	}
	if env["synced"] != "on" {
		t.Fatalf("synced ENV = %q, want on", env["synced"])
	}
	if env["optout"] != "off" {
		t.Fatalf("optout ENV = %q, want off", env["optout"])
	}
}

// TestRepoAddLocalOnlyKeepsOriginBearingRepoLocal proves `--local-only` works on a
// repo that has a remote — the normal case for tracking a repo on this host alone.
// The entry belongs to the local registry keyed by relpath, carrying no origin, so
// it never converges to a peer.
func TestRepoAddLocalOnlyKeepsOriginBearingRepoLocal(t *testing.T) {
	f := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.Root, "xdg"))
	dl := filepath.Join(f.Root, "data")
	if err := os.MkdirAll(dl, 0o750); err != nil {
		t.Fatalf("mkdir data loc: %v", err)
	}
	initializeProductState(t)
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = dl
		s.Settings.IdleThreshold = state.Duration(time.Nanosecond)
		return nil
	}); err != nil {
		t.Fatalf("seed default location: %v", err)
	}
	dest := f.JJClone(filepath.Join(dl, "alpha"))

	if _, _, err := runCLI(t, "repo", "add", "--local-only", "--no-env-sync", dest); err != nil {
		t.Fatalf("repo add --local-only on a repo with an origin: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	entry, ok := st.LocalRepos["alpha"]
	if !ok || !entry.Present() {
		t.Fatalf("alpha not in the local registry keyed by relpath: %+v", st.LocalRepos)
	}
	if entry.Value.Relpath != "alpha" || !entry.Value.LocalOnly {
		t.Fatalf("local entry = %+v, want relpath alpha and local_only true", entry.Value)
	}
	for origin := range st.Repos {
		t.Fatalf("alpha reached the propagating registry keyed by %q, want local-only", origin)
	}
	if got := st.PropagatingRepos(); len(got) != 0 {
		t.Fatalf("PropagatingRepos = %+v, want none", got)
	}

	all := st.AllRepos()
	if len(all) != 1 {
		t.Fatalf("AllRepos = %+v, want exactly alpha", all)
	}
	if !all[0].LocalOnly || all[0].Origin != "" {
		t.Fatalf("AllRepos[0] = %+v, want LocalOnly true and no origin", all[0])
	}

	stdout, _, err := runCLI(t, "repo", "ls")
	if err != nil {
		t.Fatalf("repo ls: %v", err)
	}
	if !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "(local-only)") {
		t.Fatalf("repo ls did not render the local-only repo:\n%s", stdout)
	}
}

// TestRepoAddNoEnvSyncPersistsFlag proves `repo add --no-env-sync` registers the repo
// with the env opt-out persisted. The checkout is pre-created so the add's reconcile
// finds it present rather than cloning afresh.
func TestRepoAddNoEnvSyncPersistsFlag(t *testing.T) {
	f := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.Root, "xdg"))
	dl := filepath.Join(f.Root, "data")
	if err := os.MkdirAll(dl, 0o750); err != nil {
		t.Fatalf("mkdir data loc: %v", err)
	}
	initializeProductState(t)
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = dl
		s.Settings.IdleThreshold = state.Duration(time.Nanosecond)
		return nil
	}); err != nil {
		t.Fatalf("seed default location: %v", err)
	}
	dest := f.JJClone(filepath.Join(dl, "alpha"))

	if _, _, err := runCLI(t, "repo", "add", "--no-env-sync", dest); err != nil {
		t.Fatalf("repo add --no-env-sync: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	var alpha state.Repo
	found := false
	for _, r := range st.AllRepos() {
		if r.Relpath == "alpha" {
			alpha = r
			found = true
		}
	}
	if !found {
		t.Fatalf("alpha not registered after repo add: %+v", st.AllRepos())
	}
	if !alpha.NoEnvSync {
		t.Fatal("repo add --no-env-sync did not persist NoEnvSync=true")
	}
}
