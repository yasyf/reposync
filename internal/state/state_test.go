package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/synckit/hostregistry"
)

const (
	configSubdir = "reposync"
	stateFile    = "state.json"
	lockFile     = "reconcile.lock"
)

func TestLoadMissingReturnsDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultLocation != "~/Code" {
		t.Errorf("DefaultLocation = %q, want ~/Code", s.DefaultLocation)
	}
	if got := time.Duration(s.Settings.Interval); got != 15*time.Minute {
		t.Errorf("Interval = %s, want 15m", got)
	}
	if got := time.Duration(s.Settings.IdleThreshold); got != 5*time.Minute {
		t.Errorf("IdleThreshold = %s, want 5m", got)
	}
	if got := time.Duration(s.Settings.WatchDebounce); got != 3*time.Second {
		t.Errorf("WatchDebounce = %s, want 3s", got)
	}
	if got := time.Duration(s.Settings.RepoOpTimeout); got != 2*time.Minute {
		t.Errorf("RepoOpTimeout = %s, want 2m", got)
	}
	if got := time.Duration(s.Settings.PushAfter); got != 24*time.Hour {
		t.Errorf("PushAfter = %s, want 24h", got)
	}
}

func TestLoadAppliesDefaultsToZeroSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg := filepath.Join(dir, configSubdir)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"default_location":"~/Work","settings":{"interval":"30m"}}`
	if err := os.WriteFile(filepath.Join(cfg, stateFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DefaultLocation != "~/Work" {
		t.Errorf("DefaultLocation = %q, want ~/Work", s.DefaultLocation)
	}
	if got := time.Duration(s.Settings.Interval); got != 30*time.Minute {
		t.Errorf("Interval = %s, want 30m", got)
	}
	if got := time.Duration(s.Settings.IdleThreshold); got != 5*time.Minute {
		t.Errorf("IdleThreshold = %s, want default 5m", got)
	}
	if got := time.Duration(s.Settings.WatchDebounce); got != 3*time.Second {
		t.Errorf("WatchDebounce = %s, want default 3s", got)
	}
	if got := time.Duration(s.Settings.RepoOpTimeout); got != 2*time.Minute {
		t.Errorf("RepoOpTimeout = %s, want default 2m", got)
	}
	if got := time.Duration(s.Settings.PushAfter); got != 24*time.Hour {
		t.Errorf("PushAfter = %s, want default 24h", got)
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg := filepath.Join(dir, configSubdir)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load: want error on malformed JSON, got nil")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	want := &State{
		DefaultLocation: "~/Code",
		Repos: []Repo{
			{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main", LocalOnly: false},
			{Relpath: "scratch", Origin: "", Trunk: "", LocalOnly: true},
		},
		Settings: Settings{
			Interval:      Duration(15 * time.Minute),
			IdleThreshold: Duration(5 * time.Minute),
			WatchDebounce: Duration(3 * time.Second),
			RepoOpTimeout: Duration(2 * time.Minute),
			PushAfter:     Duration(24 * time.Hour),
		},
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DefaultLocation != want.DefaultLocation {
		t.Errorf("DefaultLocation = %q, want %q", got.DefaultLocation, want.DefaultLocation)
	}
	if len(got.Repos) != 2 {
		t.Fatalf("Repos len = %d, want 2", len(got.Repos))
	}
	if got.Repos[0] != want.Repos[0] {
		t.Errorf("Repos[0] = %+v, want %+v", got.Repos[0], want.Repos[0])
	}
	if got.Repos[1] != want.Repos[1] {
		t.Errorf("Repos[1] = %+v, want %+v", got.Repos[1], want.Repos[1])
	}
	if got.Settings != want.Settings {
		t.Errorf("Settings = %+v, want %+v", got.Settings, want.Settings)
	}
}

func TestDurationMarshal(t *testing.T) {
	cases := []struct {
		id   string
		in   time.Duration
		want string
	}{
		{"minutes", 15 * time.Minute, `"15m0s"`},
		{"seconds", 3 * time.Second, `"3s"`},
		{"idle", 5 * time.Minute, `"5m0s"`},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			out, err := json.Marshal(Duration(c.in))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(out) != c.want {
				t.Errorf("Marshal = %s, want %s", out, c.want)
			}
			var back Duration
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if time.Duration(back) != c.in {
				t.Errorf("round-trip = %s, want %s", time.Duration(back), c.in)
			}
		})
	}
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	cases := []struct {
		id  string
		raw string
	}{
		{"garbage", `"notaduration"`},
		{"number", `15`},
		{"empty", `""`},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var d Duration
			if err := json.Unmarshal([]byte(c.raw), &d); err == nil {
				t.Errorf("Unmarshal(%s): want error, got nil", c.raw)
			}
		})
	}
}

func TestSaveAtomicNoLeftoversAndPerms(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s := &State{}
	s.applyDefaults()
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Save now writes under the shared reconcile lock, so the lock file is an
	// expected sibling; what must not appear is a leftover temp file, and the
	// only payload file must be state.json.
	payload := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
		if e.Name() == lockFile {
			continue
		}
		payload = append(payload, e.Name())
	}
	if len(payload) != 1 || payload[0] != stateFile {
		t.Errorf("dir payload entries = %v, want [%s]", payload, stateFile)
	}

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state perms = %o, want 600", perm)
	}
}

func TestUpsertRepoReplacesByOrigin(t *testing.T) {
	s := &State{}
	s.UpsertRepo(Repo{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"})
	s.UpsertRepo(Repo{Relpath: "moved/cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "master"})

	if len(s.Repos) != 1 {
		t.Fatalf("Repos len = %d, want 1 (dedup by origin)", len(s.Repos))
	}
	if s.Repos[0].Relpath != "moved/cc-review" || s.Repos[0].Trunk != "master" {
		t.Errorf("Repos[0] = %+v, want replaced", s.Repos[0])
	}
}

func TestUpsertRepoLocalOnlyDedupByRelpath(t *testing.T) {
	s := &State{}
	s.UpsertRepo(Repo{Relpath: "scratch", LocalOnly: true})
	s.UpsertRepo(Repo{Relpath: "scratch", Trunk: "main", LocalOnly: true})

	if len(s.Repos) != 1 {
		t.Fatalf("Repos len = %d, want 1 (dedup by relpath)", len(s.Repos))
	}
	if s.Repos[0].Trunk != "main" {
		t.Errorf("Repos[0].Trunk = %q, want main", s.Repos[0].Trunk)
	}
}

func TestFindRepoByOrigin(t *testing.T) {
	s := &State{Repos: []Repo{
		{Relpath: "a", Origin: "https://example.com/a.git"},
		{Relpath: "b", Origin: "https://example.com/b.git"},
	}}
	got, ok := s.FindRepoByOrigin("https://example.com/b.git")
	if !ok {
		t.Fatal("FindRepoByOrigin: want found")
	}
	if got.Relpath != "b" {
		t.Errorf("Relpath = %q, want b", got.Relpath)
	}
	if _, ok := s.FindRepoByOrigin("https://example.com/missing.git"); ok {
		t.Error("FindRepoByOrigin: want not found for missing origin")
	}
}

func TestRemoveRepo(t *testing.T) {
	s := &State{Repos: []Repo{
		{Relpath: "a"}, {Relpath: "b"}, {Relpath: "c"},
	}}
	s.RemoveRepo("b")
	if len(s.Repos) != 2 {
		t.Fatalf("Repos len = %d, want 2", len(s.Repos))
	}
	for _, r := range s.Repos {
		if r.Relpath == "b" {
			t.Errorf("b still present: %v", s.Repos)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		id   string
		in   string
		want string
	}{
		{"tilde-slash", "~/Code", filepath.Join(home, "Code")},
		{"bare-tilde", "~", home},
		{"absolute", "/var/data", "/var/data"},
		{"no-tilde", "relative/path", "relative/path"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got, err := expandHome(c.in)
			if err != nil {
				t.Fatalf("expandHome: %v", err)
			}
			if got != c.want {
				t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDefaultLocationExpanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := &State{DefaultLocation: "~/Code"}
	got, err := s.DefaultLocationExpanded()
	if err != nil {
		t.Fatalf("DefaultLocationExpanded: %v", err)
	}
	want := filepath.Join(home, "Code")
	if got != want {
		t.Errorf("DefaultLocationExpanded = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("DefaultLocationExpanded = %q, want absolute", got)
	}
}

func TestRepoAbsPath(t *testing.T) {
	r := Repo{Relpath: "Forge/private-ai"}
	got := r.AbsPath("/Users/yasyf/Code")
	want := "/Users/yasyf/Code/Forge/private-ai"
	if got != want {
		t.Errorf("AbsPath = %q, want %q", got, want)
	}
}

func TestUpdatePersistsMutation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	out, err := Update(context.Background(), func(s *State) error {
		s.UpsertRepo(Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(out.Repos) != 1 || out.Repos[0].Relpath != "cc-review" {
		t.Fatalf("returned state Repos = %+v, want one cc-review repo", out.Repos)
	}

	persisted, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(persisted.Repos) != 1 || persisted.Repos[0].Relpath != "cc-review" {
		t.Fatalf("persisted Repos = %+v, want one cc-review repo", persisted.Repos)
	}
}

func TestUpdateFnErrorAbortsSave(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	sentinel := errors.New("boom")
	out, err := Update(context.Background(), func(s *State) error {
		s.UpsertRepo(Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update err = %v, want sentinel", err)
	}
	if out != nil {
		t.Fatalf("Update returned state = %+v, want nil on fn error", out)
	}

	persisted, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(persisted.Repos) != 0 {
		t.Fatalf("persisted Repos = %+v, want none (save aborted)", persisted.Repos)
	}
}

func TestUpdateConcurrentNoLostUpdates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			relpath := fmt.Sprintf("repo-%02d", i)
			origin := fmt.Sprintf("https://example.com/repo-%02d.git", i)
			_, errs[i] = Update(context.Background(), func(s *State) error {
				s.UpsertRepo(Repo{Relpath: relpath, Origin: origin, Trunk: "main"})
				return nil
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Update goroutine %d: %v", i, err)
		}
	}

	persisted, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(persisted.Repos) != n {
		t.Fatalf("persisted Repos count = %d, want %d (lost updates)", len(persisted.Repos), n)
	}
	seen := make(map[string]bool, n)
	for _, r := range persisted.Repos {
		seen[r.Origin] = true
	}
	for i := 0; i < n; i++ {
		origin := fmt.Sprintf("https://example.com/repo-%02d.git", i)
		if !seen[origin] {
			t.Errorf("repo %s dropped from persisted state", origin)
		}
	}
}

// TestUpdatePreservesIdentityKeysBothDirections is the lost-update regression: a
// reposync write (state.Update / Save) must leave the self/hosts identity that
// hostregistry owns byte-for-byte intact, and a hostregistry write must leave
// reposync's repos/settings/default_location intact. Both writers share one file
// through the FK-preserving Config.UpdateRaw, so neither clobbers the other.
func TestUpdatePreservesIdentityKeysBothDirections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Seed the identity slice via hostregistry, then reposync's slice via Update.
	if _, err := Config.Update(context.Background(), func(g *hostregistry.Registry) error {
		g.Self = "yasyf@self"
		g.UpsertHost("yasyf@peer")
		return nil
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if _, err := Update(context.Background(), func(s *State) error {
		s.DefaultLocation = "~/Work"
		s.UpsertRepo(Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		return nil
	}); err != nil {
		t.Fatalf("seed reposync state: %v", err)
	}

	identityKeys := []string{"self", "hosts"}
	domainKeys := []string{"repos", "settings", "default_location"}

	// Direction 1: a reposync write mutates only its keys; self/hosts stay byte-identical.
	before := readRawState(t)
	if _, err := Update(context.Background(), func(s *State) error {
		s.UpsertRepo(Repo{Relpath: "notes", Origin: "https://example.com/notes.git", Trunk: "main"})
		return nil
	}); err != nil {
		t.Fatalf("reposync Update: %v", err)
	}
	after := readRawState(t)
	assertRawKeysEqual(t, "reposync write", before, after, identityKeys)
	assertRawKeysChanged(t, "reposync write", before, after, []string{"repos"})

	// Direction 2: a hostregistry write mutates only self/hosts; reposync's keys stay byte-identical.
	before = readRawState(t)
	if _, err := Config.Update(context.Background(), func(g *hostregistry.Registry) error {
		g.UpsertHost("yasyf@peer2")
		g.Self = "yasyf@self2"
		return nil
	}); err != nil {
		t.Fatalf("hostregistry Update: %v", err)
	}
	after = readRawState(t)
	assertRawKeysEqual(t, "hostregistry write", before, after, domainKeys)
	assertRawKeysChanged(t, "hostregistry write", before, after, identityKeys)

	// Sanity: the final file still parses into a coherent reposync State and Registry.
	st, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Repos) != 2 || st.DefaultLocation != "~/Work" {
		t.Fatalf("reposync keys corrupted: %+v", st)
	}
	reg, err := Config.Load()
	if err != nil {
		t.Fatalf("registry Load: %v", err)
	}
	if reg.Self != "yasyf@self2" || !sliceContains(reg.Hosts, "yasyf@peer") || !sliceContains(reg.Hosts, "yasyf@peer2") {
		t.Fatalf("identity keys corrupted: %+v", reg)
	}
}

func readRawState(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return raw
}

func assertRawKeysEqual(t *testing.T, label string, before, after map[string]json.RawMessage, keys []string) {
	t.Helper()
	for _, key := range keys {
		if !equalRaw(before[key], after[key]) {
			t.Fatalf("%s: %s changed (not byte-for-byte preserved):\n before: %s\n  after: %s", label, key, before[key], after[key])
		}
	}
}

func assertRawKeysChanged(t *testing.T, label string, before, after map[string]json.RawMessage, keys []string) {
	t.Helper()
	for _, key := range keys {
		if equalRaw(before[key], after[key]) {
			t.Fatalf("%s: %s did not change, want the write to have updated it: %s", label, key, after[key])
		}
	}
}

func equalRaw(a, b json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sliceContains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
