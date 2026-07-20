package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/synckit/manifest"
)

func TestReposyncManifestUsesStrictSchema(t *testing.T) {
	m := reposyncManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(payload, []byte(`"backend"`)) || bytes.Contains(payload, []byte(`"launchd"`)) {
		t.Fatalf("manifest contains removed fields: %s", payload)
	}
	path := filepath.Join(t.TempDir(), "reposync.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("strict Load: %v", err)
	}
	if time.Duration(loaded.Watch.Debounce) != watchDebounce {
		t.Fatalf("watch debounce = %v, want %v", time.Duration(loaded.Watch.Debounce), watchDebounce)
	}
	if loaded.Service.Transport != "stdio" || len(loaded.Service.ServeArgs) != 1 || loaded.Service.ServeArgs[0] != "rpc-serve" {
		t.Fatalf("service = %+v, want stdio rpc-serve", loaded.Service)
	}
}
