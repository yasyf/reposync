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

	"github.com/gofrs/flock"
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
	if s.Self != "" {
		t.Errorf("Self = %q, want empty", s.Self)
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
		Self:            "yasyf@yasyf",
		Hosts:           []string{"yasyf@yasyf-home"},
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
	if got.Self != want.Self {
		t.Errorf("Self = %q, want %q", got.Self, want.Self)
	}
	if len(got.Hosts) != 1 || got.Hosts[0] != "yasyf@yasyf-home" {
		t.Errorf("Hosts = %v, want [yasyf@yasyf-home]", got.Hosts)
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
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	if len(names) != 1 || names[0] != stateFile {
		t.Errorf("dir entries = %v, want [%s]", names, stateFile)
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

func TestUpsertHostNoDupes(t *testing.T) {
	s := &State{}
	s.UpsertHost("yasyf@yasyf-home")
	s.UpsertHost("yasyf@yasyf-home")
	s.UpsertHost("yasyf@yasyf-work")

	if len(s.Hosts) != 2 {
		t.Fatalf("Hosts = %v, want 2 entries", s.Hosts)
	}
}

func TestRemoveHost(t *testing.T) {
	s := &State{Hosts: []string{"a", "b", "c"}}
	s.RemoveHost("b")
	if len(s.Hosts) != 2 || s.Hosts[0] != "a" || s.Hosts[1] != "c" {
		t.Errorf("Hosts = %v, want [a c]", s.Hosts)
	}
}

func TestWithLockRunsFnAndCreatesLockFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	ran := false
	if err := WithLock(context.Background(), func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !ran {
		t.Error("WithLock: fn did not run")
	}

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); err != nil {
		t.Errorf("lock file missing: %v", err)
	}

	if err := WithLock(context.Background(), func() error { return nil }); err != nil {
		t.Errorf("second WithLock: %v", err)
	}
}

func TestWithLockContendedReturnsErrLockBusy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Hold the lock via an independent flock handle on the same file, simulating
	// another process holding the reconcile lock.
	holder := flock.New(filepath.Join(dir, lockFile))
	locked, err := holder.TryLock()
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	if !locked {
		t.Fatal("could not acquire lock to hold")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	ran := false
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- WithLock(ctx, func() error {
			ran = true
			return nil
		})
	}()

	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("contended WithLock took %s, want fast failure", elapsed)
		}
		if !errors.Is(err, ErrLockBusy) {
			t.Fatalf("WithLock err = %v, want ErrLockBusy", err)
		}
		if ran {
			t.Error("fn ran despite the lock being held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contended WithLock blocked past its deadline")
	}

	// Release the held lock; a fresh acquire must now succeed.
	if err := holder.Unlock(); err != nil {
		t.Fatalf("release held lock: %v", err)
	}
	acquired := false
	if err := WithLock(context.Background(), func() error {
		acquired = true
		return nil
	}); err != nil {
		t.Fatalf("WithLock after release: %v", err)
	}
	if !acquired {
		t.Error("fn did not run after the lock was released")
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
