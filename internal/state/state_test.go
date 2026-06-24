package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

	want := New()
	want.DefaultLocation = "~/Code"
	want.Settings = Settings{
		Interval:      Duration(15 * time.Minute),
		IdleThreshold: Duration(5 * time.Minute),
		WatchDebounce: Duration(3 * time.Second),
		RepoOpTimeout: Duration(2 * time.Minute),
		PushAfter:     Duration(24 * time.Hour),
	}
	want.AddRepo(Repo{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"})
	want.AddRepo(Repo{Relpath: "scratch", LocalOnly: true})
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
	if !reflect.DeepEqual(got.Repos, want.Repos) {
		t.Errorf("Repos = %+v, want %+v", got.Repos, want.Repos)
	}
	if !reflect.DeepEqual(got.LocalRepos, want.LocalRepos) {
		t.Errorf("LocalRepos = %+v, want %+v", got.LocalRepos, want.LocalRepos)
	}
	// The flat view round-trips both the propagating and local-only repos.
	all := got.AllRepos()
	byPath := make(map[string]Repo, len(all))
	for _, r := range all {
		byPath[r.Relpath] = r
	}
	if r := byPath["cc-review"]; r.Origin != "https://github.com/yasyf/cc-review.git" || r.Trunk != "main" || r.LocalOnly {
		t.Errorf("cc-review round-trip = %+v", r)
	}
	if r := byPath["scratch"]; r.Origin != "" || !r.LocalOnly {
		t.Errorf("scratch round-trip = %+v", r)
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

// pinClock pins state.Now to a controllable stamp so registry adds and removes get
// deterministic, strictly-increasing microsecond stamps in a test.
func pinClock(t *testing.T) func() {
	t.Helper()
	saved := Now
	t.Cleanup(func() { Now = saved })
	base := time.Unix(1_700_000_000, 0)
	step := 0
	return func() {
		Now = func() time.Time {
			step++
			return base.Add(time.Duration(step) * time.Second)
		}
	}
}

func TestAddRepoKeysPropagatingByOrigin(t *testing.T) {
	pinClock(t)()
	s := New()
	s.AddRepo(Repo{Relpath: "cc-review", Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"})

	origin := "https://github.com/yasyf/cc-review.git"
	e, ok := s.Repos[origin]
	if !ok {
		t.Fatalf("repo not keyed by origin in propagating registry: %v", s.Repos)
	}
	if !e.Present() || e.Value.Relpath != "cc-review" || e.Value.Trunk != "main" || e.Value.LocalOnly {
		t.Fatalf("entry = %+v, want present cc-review/main not-local", e)
	}
	if len(s.LocalRepos) != 0 {
		t.Fatalf("origin repo leaked into the local registry: %v", s.LocalRepos)
	}
}

// TestAddRepoReaddAfterTombstoneWins proves the registry's re-add semantics: a repo
// removed (tombstoned) then re-added with a strictly-later stamp is present again,
// carrying the new payload — the LWW-Element-Set guarantee.
func TestAddRepoReaddAfterTombstoneWins(t *testing.T) {
	pinClock(t)()
	s := New()
	origin := "https://github.com/yasyf/cc-review.git"

	s.AddRepo(Repo{Relpath: "cc-review", Origin: origin, Trunk: "main"})
	s.RemoveRepo("cc-review")
	if s.Repos[origin].Present() {
		t.Fatal("repo still present after tombstone")
	}
	s.AddRepo(Repo{Relpath: "moved/cc-review", Origin: origin, Trunk: "master"})

	e := s.Repos[origin]
	if !e.Present() {
		t.Fatal("re-add after tombstone did not restore presence")
	}
	if e.Value.Relpath != "moved/cc-review" || e.Value.Trunk != "master" {
		t.Fatalf("re-add value = %+v, want moved/cc-review master", e.Value)
	}
}

func TestAddRepoLocalOnlyKeyedByRelpath(t *testing.T) {
	pinClock(t)()
	s := New()
	s.AddRepo(Repo{Relpath: "scratch", LocalOnly: true})
	s.AddRepo(Repo{Relpath: "scratch", Trunk: "main", LocalOnly: true})

	if len(s.LocalRepos) != 1 {
		t.Fatalf("LocalRepos len = %d, want 1 (keyed by relpath)", len(s.LocalRepos))
	}
	e := s.LocalRepos["scratch"]
	if !e.Present() || e.Value.Trunk != "main" {
		t.Fatalf("local entry = %+v, want present with later trunk=main", e)
	}
	if len(s.Repos) != 0 {
		t.Fatalf("local-only repo leaked into the propagating registry: %v", s.Repos)
	}
}

func TestFindRepoByOrigin(t *testing.T) {
	pinClock(t)()
	s := New()
	s.AddRepo(Repo{Relpath: "a", Origin: "https://example.com/a.git"})
	s.AddRepo(Repo{Relpath: "b", Origin: "https://example.com/b.git"})

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

	// A tombstoned repo is not found.
	s.RemoveRepo("a")
	if _, ok := s.FindRepoByOrigin("https://example.com/a.git"); ok {
		t.Error("FindRepoByOrigin: want not found for a tombstoned repo")
	}
}

// TestRemoveRepoTombstonesNotDrops is the removal-fix regression: rm keeps the entry
// and stamps removed_at later than added_at, so the entry stays in the registry to
// propagate the removal — it is not dropped from the map.
func TestRemoveRepoTombstonesNotDrops(t *testing.T) {
	pinClock(t)()
	s := New()
	origin := "https://example.com/b.git"
	s.AddRepo(Repo{Relpath: "a", Origin: "https://example.com/a.git"})
	s.AddRepo(Repo{Relpath: "b", Origin: origin})

	s.RemoveRepo("b")

	e, ok := s.Repos[origin]
	if !ok {
		t.Fatal("rm dropped the entry instead of tombstoning it: removal would not propagate")
	}
	if e.Present() {
		t.Fatal("removed repo still present")
	}
	if e.Removed <= e.Added {
		t.Fatalf("tombstone stamp not later than add: added=%d removed=%d", e.Added, e.Removed)
	}
	// The flat present view excludes the tombstone but keeps the live repo.
	all := s.AllRepos()
	if len(all) != 1 || all[0].Relpath != "a" {
		t.Fatalf("AllRepos after rm = %+v, want only [a]", all)
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

	origin := "https://example.com/cc-review.git"
	out, err := Update(context.Background(), func(s *State) error {
		s.AddRepo(Repo{Relpath: "cc-review", Origin: origin, Trunk: "main"})
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if e, ok := out.Repos[origin]; !ok || e.Value.Relpath != "cc-review" {
		t.Fatalf("returned state Repos = %+v, want one cc-review repo keyed by origin", out.Repos)
	}

	persisted, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e, ok := persisted.Repos[origin]; !ok || !e.Present() || e.Value.Relpath != "cc-review" {
		t.Fatalf("persisted Repos = %+v, want one present cc-review repo", persisted.Repos)
	}
}

func TestUpdateFnErrorAbortsSave(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	sentinel := errors.New("boom")
	out, err := Update(context.Background(), func(s *State) error {
		s.AddRepo(Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
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
				s.AddRepo(Repo{Relpath: relpath, Origin: origin, Trunk: "main"})
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
	for i := 0; i < n; i++ {
		origin := fmt.Sprintf("https://example.com/repo-%02d.git", i)
		if e, ok := persisted.Repos[origin]; !ok || !e.Present() {
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
		s.AddRepo(Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		return nil
	}); err != nil {
		t.Fatalf("seed reposync state: %v", err)
	}

	identityKeys := []string{"self", "hosts"}
	domainKeys := []string{"repos", "local_repos", "settings", "default_location"}

	// Direction 1: a reposync write mutates only its keys; self/hosts stay byte-identical.
	before := readRawState(t)
	if _, err := Update(context.Background(), func(s *State) error {
		s.AddRepo(Repo{Relpath: "notes", Origin: "https://example.com/notes.git", Trunk: "main"})
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
