package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/hostregistry"
	"github.com/yasyf/synckit/manifest"

	"github.com/yasyf/reposync/internal/state"
)

// seedRegistry points the state file at a temp config dir and writes a known
// self+hosts identity into the shared synckit mesh.
func seedRegistry(t *testing.T, self string, hosts ...string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := hostregistry.Mesh.Update(t.Context(), func(g *hostregistry.Registry) error {
		g.Self = self
		for _, h := range hosts {
			g.UpsertHost(h)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
}

// runCLI executes the root command with args, capturing stdout and stderr
// separately so the --json contract (JSON only on stdout) can be asserted.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCLIStdin(t, "", args...)
}

// runCLIStdin is runCLI with stdin fed from in, for commands like state apply-json
// that read a payload from stdin.
func runCLIStdin(t *testing.T, in string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	root := newRoot("test")
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(in))
	err = root.ExecuteContext(t.Context())
	return out.String(), errBuf.String(), err
}

func TestSelfJSONShape(t *testing.T) {
	seedRegistry(t, "yasyf@laptop", "yasyf@desktop")

	stdout, stderr, err := runCLI(t, "self", "--json")
	if err != nil {
		t.Fatalf("self --json: %v", err)
	}
	if stderr != "" {
		t.Fatalf("self --json wrote to stderr: %q", stderr)
	}

	want := `{"version":1,"self":"yasyf@laptop"}`
	if got := strings.TrimRight(stdout, "\n"); got != want {
		t.Fatalf("self --json:\n got: %s\nwant: %s", got, want)
	}

	// The version field must be the literal int 1, not "1" or 1.0.
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("self --json output is not valid JSON: %v", err)
	}
	if v, ok := payload["version"].(float64); !ok || v != 1 {
		t.Fatalf("version = %v (%T), want literal 1", payload["version"], payload["version"])
	}
}

func TestSelfPayloadMarshalsKnownRegistry(t *testing.T) {
	// Golden marshal: the Go payload type must encode to the exact bytes a
	// cross-language consumer pins to, independent of any command plumbing.
	selfGolden, err := json.Marshal(selfPayload{Version: jsonVersion, Self: "yasyf@laptop"})
	if err != nil {
		t.Fatalf("marshal selfPayload: %v", err)
	}
	if got, want := string(selfGolden), `{"version":1,"self":"yasyf@laptop"}`; got != want {
		t.Fatalf("selfPayload golden:\n got: %s\nwant: %s", got, want)
	}
}

func TestStateGetJSONEmitsPropagatingRegistryOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.AddRepo(state.Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		s.AddRepo(state.Repo{Relpath: "scratch", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	stdout, stderr, err := runCLI(t, "state", "get-json")
	if err != nil {
		t.Fatalf("state get-json: %v", err)
	}
	if stderr != "" {
		t.Fatalf("state get-json wrote to stderr: %q", stderr)
	}

	var reg map[string]struct {
		AddedAt   int64 `json:"added_at"`
		RemovedAt int64 `json:"removed_at"`
		Value     struct {
			Relpath   string `json:"relpath"`
			LocalOnly bool   `json:"local_only"`
		} `json:"value"`
	}
	if err := json.Unmarshal([]byte(stdout), &reg); err != nil {
		t.Fatalf("state get-json output is not a registry object: %v\n%s", err, stdout)
	}
	if _, ok := reg["https://example.com/cc-review.git"]; !ok {
		t.Fatalf("propagating repo missing from get-json: %s", stdout)
	}
	// Local-only repos must never appear in the cross-host wire form.
	if strings.Contains(stdout, "scratch") {
		t.Fatalf("local-only repo leaked into state get-json: %s", stdout)
	}
}

func TestJSONVersionIsLiteralOne(t *testing.T) {
	if jsonVersion != 1 {
		t.Fatalf("jsonVersion = %d, want literal 1 (a bump breaks the cross-language contract)", jsonVersion)
	}
}

func TestStatePathUnderTempConfig(t *testing.T) {
	// Guards the test seeding itself: state.json must land under the temp
	// XDG_CONFIG_HOME so these tests never touch the real config.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := state.Config.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if filepath.Base(path) != "state.json" {
		t.Fatalf("state path = %q, want it to end in state.json", path)
	}
}

// TestStateApplyJSONPreservesLocalReposAndSettings is the two-registry-scoping guard:
// applying a merged propagating registry must replace ONLY the propagating Repos,
// leaving the local-only repos, settings, and default location byte-for-byte intact.
func TestStateApplyJSONPreservesLocalReposAndSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Seed a host that already tracks a local-only repo and a non-default location.
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = "~/work"
		s.AddRepo(state.Repo{Relpath: "scratch", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("seed local state: %v", err)
	}
	before, err := state.Load()
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	// A merged propagating registry as synckitd would pipe in: one origin-keyed repo.
	merged := cregistry.New[state.RepoMeta]()
	merged.Add("https://example.com/cc-review.git", state.RepoMeta{Relpath: "cc-review", Trunk: "main"}, 100)
	payload, err := json.Marshal(merged)
	if err != nil {
		t.Fatalf("marshal merged: %v", err)
	}

	stdout, stderr, err := runCLIStdin(t, string(payload), "state", "apply-json")
	if err != nil {
		t.Fatalf("state apply-json: %v", err)
	}
	if stderr != "" {
		t.Fatalf("state apply-json wrote to stderr: %q", stderr)
	}
	if got, want := strings.TrimRight(stdout, "\n"), `{"applied":1}`; got != want {
		t.Fatalf("state apply-json:\n got: %s\nwant: %s", got, want)
	}

	after, err := state.Load()
	if err != nil {
		t.Fatalf("load after: %v", err)
	}

	// The propagating registry was replaced with the merged payload.
	if e, ok := after.Repos["https://example.com/cc-review.git"]; !ok || !e.Present() || e.Value.Relpath != "cc-review" {
		t.Fatalf("propagating repo not applied: %v", after.Repos)
	}
	// The local-only registry, settings, and default location survived untouched.
	if e, ok := after.LocalRepos["scratch"]; !ok || !e.Present() {
		t.Fatalf("apply-json clobbered the local-only registry: %v", after.LocalRepos)
	}
	if after.DefaultLocation != "~/work" {
		t.Fatalf("apply-json changed default_location: got %q, want ~/work", after.DefaultLocation)
	}
	if after.Settings != before.Settings {
		t.Fatalf("apply-json changed settings:\n got: %+v\nwant: %+v", after.Settings, before.Settings)
	}
}

// TestStateApplyJSONFingerprintIsApplyStable proves apply-json writes only state.json
// metadata, never refs: applying a payload twice is idempotent and the propagating
// registry contents (which never carry a fingerprint) round-trip unchanged.
func TestStateApplyJSONFingerprintIsApplyStable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	merged := cregistry.New[state.RepoMeta]()
	merged.Add("https://example.com/a.git", state.RepoMeta{Relpath: "a", Trunk: "main"}, 100)
	payload, err := json.Marshal(merged)
	if err != nil {
		t.Fatalf("marshal merged: %v", err)
	}

	for i := range 2 {
		stdout, _, err := runCLIStdin(t, string(payload), "state", "apply-json")
		if err != nil {
			t.Fatalf("apply-json pass %d: %v", i, err)
		}
		if got, want := strings.TrimRight(stdout, "\n"), `{"applied":1}`; got != want {
			t.Fatalf("apply-json pass %d:\n got: %s\nwant: %s", i, got, want)
		}
	}
}

// TestListJSONCoversBothRegistries proves list --json emits one watch item per repo
// from BOTH registries — propagating repos keyed by origin and local-only repos keyed
// by relpath — and reports an empty (not dropped) fingerprint for an uncloned repo.
func TestListJSONCoversBothRegistries(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = t.TempDir() // empty location: no repo is cloned
		s.AddRepo(state.Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		s.AddRepo(state.Repo{Relpath: "scratch", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	stdout, _, err := runCLI(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}

	var items []manifest.WatchItem
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("list --json output is not a WatchItem array: %v\n%s", err, stdout)
	}
	byID := map[string]manifest.WatchItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	prop, ok := byID["https://example.com/cc-review.git"]
	if !ok {
		t.Fatalf("propagating repo (origin id) missing from list: %s", stdout)
	}
	if _, ok := byID["scratch"]; !ok {
		t.Fatalf("local-only repo (relpath id) missing from list: %s", stdout)
	}
	if len(items) != 2 {
		t.Fatalf("list --json emitted %d items, want 2 (both registries): %s", len(items), stdout)
	}
	// An uncloned repo keeps its watch dirs but reports an empty fingerprint.
	if prop.Fingerprint != "" {
		t.Fatalf("uncloned repo fingerprint = %q, want empty", prop.Fingerprint)
	}
	if len(prop.WatchDirs) == 0 {
		t.Fatalf("propagating repo has no watch dirs: %+v", prop)
	}
}
