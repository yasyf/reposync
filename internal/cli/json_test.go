package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/hostregistry"
	"github.com/yasyf/synckit/rpc"
	"github.com/yasyf/synckit/syncservice"

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
	var out, errBuf bytes.Buffer
	root := newRoot("test")
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	err = root.ExecuteContext(t.Context())
	return out.String(), errBuf.String(), err
}

// pipeTransport adapts the exact persistent rpc client to syncservice's typed
// response envelope for in-process dispatcher tests.
type pipeTransport struct{ client *rpc.Client }

func (t *pipeTransport) Do(ctx context.Context, req *rpc.Request) (*syncservice.Response, error) {
	response, err := t.client.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	return &syncservice.Response{
		OK: response.OK, Result: response.Result, Error: response.Error,
	}, nil
}

func (t *pipeTransport) Close() error { return t.client.Close() }

// serveConsumer wires a repoConsumer behind the exact spawned-session engine
// and returns a typed client speaking to it.
func serveConsumer(t *testing.T) *syncservice.Client {
	t.Helper()
	d := rpc.NewDispatcher()
	syncservice.RegisterConsumer(d, repoConsumer{})
	c := syncservice.NewClient(servePipeDispatcher(t, d))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// servePipe wires the full rpc-serve dispatcher (typed contract plus env.get_state)
// behind the exact spawned-session engine and returns the raw transport.
func servePipe(t *testing.T) *pipeTransport {
	t.Helper()
	return servePipeDispatcher(t, newServeDispatcher())
}

func servePipeDispatcher(t *testing.T, dispatcher *rpc.Dispatcher) *pipeTransport {
	t.Helper()
	server, clientConn := net.Pipe()
	srvCtx, srvCancel := context.WithCancel(context.Background())
	srvDone := make(chan error, 1)
	go func() {
		srvDone <- rpc.NewServer(dispatcher).ServeSession(srvCtx, server, server)
	}()
	t.Cleanup(func() {
		srvCancel()
		_ = server.Close()
		<-srvDone
	})
	tx := &pipeTransport{client: rpc.NewClient(rpc.ClientConfig{
		Build: rpc.Build,
		Dial:  func(context.Context) (net.Conn, error) { return clientConn, nil },
	})}
	t.Cleanup(func() { _ = tx.Close() })
	return tx
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

// TestConsumerGetStateEmitsPropagatingRegistryOnly proves svc.get-state, served over
// rpc-serve, returns the propagating (origin-keyed) registry alone — the local-only
// repos never cross hosts.
func TestConsumerGetStateEmitsPropagatingRegistryOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.AddRepo(state.Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		s.AddRepo(state.Repo{Relpath: "scratch", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	raw, err := serveConsumer(t).GetState(t.Context())
	if err != nil {
		t.Fatalf("get-state: %v", err)
	}

	var reg map[string]struct {
		AddedAt   int64 `json:"added_at"`
		RemovedAt int64 `json:"removed_at"`
		Value     struct {
			Relpath   string `json:"relpath"`
			LocalOnly bool   `json:"local_only"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("get-state output is not a registry object: %v\n%s", err, raw)
	}
	if _, ok := reg["https://example.com/cc-review.git"]; !ok {
		t.Fatalf("propagating repo missing from get-state: %s", raw)
	}
	// Local-only repos must never appear in the cross-host wire form.
	if strings.Contains(string(raw), "scratch") {
		t.Fatalf("local-only repo leaked into get-state: %s", raw)
	}
}

// TestConsumerListCoversBothRegistries proves svc.list emits one watch item per repo
// from BOTH registries — propagating repos keyed by origin and local-only repos keyed
// by relpath — and reports empty (not dropped) fingerprint components for an uncloned repo.
func TestConsumerListCoversBothRegistries(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = t.TempDir() // empty location: no repo is cloned
		s.AddRepo(state.Repo{Relpath: "cc-review", Origin: "https://example.com/cc-review.git", Trunk: "main"})
		s.AddRepo(state.Repo{Relpath: "scratch", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	items, err := serveConsumer(t).List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	byID := map[string]syncservice.WatchItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	prop, ok := byID["https://example.com/cc-review.git"]
	if !ok {
		t.Fatalf("propagating repo (origin id) missing from list: %+v", items)
	}
	if _, ok := byID["scratch"]; !ok {
		t.Fatalf("local-only repo (relpath id) missing from list: %+v", items)
	}
	if len(items) != 2 {
		t.Fatalf("list emitted %d items, want 2 (both registries): %+v", len(items), items)
	}
	// An uncloned repo keeps its watch dirs and the separator between empty components.
	if prop.Fingerprint != "\n" {
		t.Fatalf("uncloned repo fingerprint = %q, want newline", prop.Fingerprint)
	}
	if len(prop.WatchDirs) == 0 {
		t.Fatalf("propagating repo has no watch dirs: %+v", prop)
	}
}

// TestConsumerCapabilities proves svc.capabilities reports reposync's identity and the
// current protocol version, the handshake synckitd checks before driving the contract.
func TestConsumerCapabilities(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	caps, err := serveConsumer(t).Capabilities(t.Context())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps.Name != state.ToolName {
		t.Fatalf("capabilities name = %q, want %q", caps.Name, state.ToolName)
	}
	if caps.ProtocolVersion != syncservice.ProtocolVersion {
		t.Fatalf("protocol version = %d, want %d", caps.ProtocolVersion, syncservice.ProtocolVersion)
	}
	if strings.Join(caps.Methods, ",") != strings.Join(syncservice.AllMethods, ",") {
		t.Fatalf("methods = %v, want %v", caps.Methods, syncservice.AllMethods)
	}
}

// TestApplyPreservesLocalReposAndSettings is the two-registry-scoping guard for the
// native in-process apply (repoDriver.SaveRegistry → SaveReposUnlocked): writing a
// merged propagating registry must replace ONLY the propagating Repos, leaving the
// local-only repos, settings, and default location byte-for-byte intact.
func TestApplyPreservesLocalReposAndSettings(t *testing.T) {
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

	// A merged propagating registry as the converge pass would persist.
	merged := cregistry.New[state.RepoMeta]()
	merged.Add("https://example.com/cc-review.git", state.RepoMeta{Relpath: "cc-review", Trunk: "main"}, 100)

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load for apply: %v", err)
	}
	if err := state.WithLock(t.Context(), func() error {
		st.Repos = merged
		return st.SaveReposUnlocked()
	}); err != nil {
		t.Fatalf("native apply: %v", err)
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
		t.Fatalf("apply clobbered the local-only registry: %v", after.LocalRepos)
	}
	if after.DefaultLocation != "~/work" {
		t.Fatalf("apply changed default_location: got %q, want ~/work", after.DefaultLocation)
	}
	if after.Settings != before.Settings {
		t.Fatalf("apply changed settings:\n got: %+v\nwant: %+v", after.Settings, before.Settings)
	}
}
