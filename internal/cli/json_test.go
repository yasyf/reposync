package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/reposync/hostregistry"
)

// seedRegistry points the state file at a temp config dir and writes a known
// self+hosts identity.
func seedRegistry(t *testing.T, self string, hosts ...string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := hostregistry.Update(t.Context(), func(g *hostregistry.Registry) error {
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

func TestHostLsJSONShape(t *testing.T) {
	seedRegistry(t, "yasyf@laptop", "yasyf@desktop", "yasyf@server")

	stdout, stderr, err := runCLI(t, "host", "ls", "--json")
	if err != nil {
		t.Fatalf("host ls --json: %v", err)
	}
	if stderr != "" {
		t.Fatalf("host ls --json wrote to stderr: %q", stderr)
	}

	want := `{"version":1,"self":"yasyf@laptop","hosts":["yasyf@desktop","yasyf@server"]}`
	if got := strings.TrimRight(stdout, "\n"); got != want {
		t.Fatalf("host ls --json:\n got: %s\nwant: %s", got, want)
	}
}

func TestHostLsJSONEmptyHosts(t *testing.T) {
	seedRegistry(t, "yasyf@laptop")

	stdout, stderr, err := runCLI(t, "host", "ls", "--json")
	if err != nil {
		t.Fatalf("host ls --json: %v", err)
	}
	if stderr != "" {
		t.Fatalf("host ls --json wrote to stderr: %q", stderr)
	}

	// Empty hosts must serialize as [], never null, and never a prose message.
	want := `{"version":1,"self":"yasyf@laptop","hosts":[]}`
	if got := strings.TrimRight(stdout, "\n"); got != want {
		t.Fatalf("host ls --json (empty):\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(stdout, "null") {
		t.Fatalf("empty hosts serialized as null, not []: %s", stdout)
	}
}

func TestHostLsHumanUnchanged(t *testing.T) {
	seedRegistry(t, "yasyf@laptop", "yasyf@desktop", "yasyf@server")

	stdout, _, err := runCLI(t, "host", "ls")
	if err != nil {
		t.Fatalf("host ls: %v", err)
	}

	// The human path is the original tabwriter listing: a HOST header then one
	// host per line, with no self and no JSON.
	want := "HOST\nyasyf@desktop\nyasyf@server\n"
	if stdout != want {
		t.Fatalf("host ls human output changed:\n got: %q\nwant: %q", stdout, want)
	}
}

func TestSelfPayloadMarshalsKnownRegistry(t *testing.T) {
	// Golden marshal: the Go payload types must encode to the exact bytes a
	// cross-language consumer pins to, independent of any command plumbing.
	selfGolden, err := json.Marshal(selfPayload{Version: jsonVersion, Self: "yasyf@laptop"})
	if err != nil {
		t.Fatalf("marshal selfPayload: %v", err)
	}
	if got, want := string(selfGolden), `{"version":1,"self":"yasyf@laptop"}`; got != want {
		t.Fatalf("selfPayload golden:\n got: %s\nwant: %s", got, want)
	}

	hostsGolden, err := json.Marshal(hostsPayload{
		Version: jsonVersion,
		Self:    "yasyf@laptop",
		Hosts:   []string{"yasyf@desktop", "yasyf@server"},
	})
	if err != nil {
		t.Fatalf("marshal hostsPayload: %v", err)
	}
	if got, want := string(hostsGolden), `{"version":1,"self":"yasyf@laptop","hosts":["yasyf@desktop","yasyf@server"]}`; got != want {
		t.Fatalf("hostsPayload golden:\n got: %s\nwant: %s", got, want)
	}

	empty, err := json.Marshal(hostsPayload{Version: jsonVersion, Self: "yasyf@laptop", Hosts: []string{}})
	if err != nil {
		t.Fatalf("marshal empty hostsPayload: %v", err)
	}
	if got, want := string(empty), `{"version":1,"self":"yasyf@laptop","hosts":[]}`; got != want {
		t.Fatalf("empty hostsPayload golden:\n got: %s\nwant: %s", got, want)
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
	path, err := hostregistry.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if filepath.Base(path) != "state.json" {
		t.Fatalf("state path = %q, want it to end in state.json", path)
	}
}
