package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/rpc"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestEnvGetStateOverWire proves env.get_state, served over the real rpc-serve path,
// returns the Observe state (with int64 mtime stamps intact) for an eligible repo,
// filters a NoEnvSync repo out, and ignores an unknown origin.
func TestEnvGetStateOverWire(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	if err := os.MkdirAll(dl, 0o750); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}

	alpha := fx.JJClone(filepath.Join(dl, "alpha"))
	if err := os.WriteFile(filepath.Join(alpha, ".env"), []byte("API_KEY=secret\nB=2\n"), 0o600); err != nil {
		t.Fatalf("write alpha .env: %v", err)
	}
	beta := fx.JJClone(filepath.Join(dl, "beta"))
	if err := os.WriteFile(filepath.Join(beta, ".env"), []byte("HIDDEN=1\n"), 0o600); err != nil {
		t.Fatalf("write beta .env: %v", err)
	}

	initializeProductState(t)
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = dl
		s.AddRepo(state.Repo{Relpath: "alpha", Origin: fx.Origin, Trunk: "main"})
		s.AddRepo(state.Repo{Relpath: "beta", Origin: "beta-origin", Trunk: "main", NoEnvSync: true})
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	tx := servePipe(t)
	resp, err := tx.Do(t.Context(), &rpc.Request{
		Method: env.MethodGetState,
		Params: map[string]any{"origins": []any{fx.Origin, "beta-origin", "unknown-origin"}},
	})
	if err != nil {
		t.Fatalf("env.get_state: %v", err)
	}
	if !resp.OK {
		t.Fatalf("env.get_state not ok: %s", resp.Error)
	}

	var payload struct {
		Repos map[string]map[string]map[string]struct {
			AddedAt int64  `json:"added_at"`
			Value   string `json:"value"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("decode env.get_state result: %v\n%s", err, resp.Result)
	}

	alphaState, ok := payload.Repos[fx.Origin]
	if !ok {
		t.Fatalf("eligible repo missing from response: %s", resp.Result)
	}
	entry := alphaState[".env"]["API_KEY"]
	if entry.Value != "secret" {
		t.Fatalf("API_KEY = %+v, want value secret", entry)
	}
	// The stamp is the file's mtime in microseconds, an int64 that must round-trip
	// exactly (a float64 hop would truncate it).
	info, err := os.Stat(filepath.Join(alpha, ".env"))
	if err != nil {
		t.Fatalf("stat .env: %v", err)
	}
	if want := info.ModTime().UnixMicro(); entry.AddedAt != want {
		t.Fatalf("API_KEY added_at = %d, want file mtime micros %d", entry.AddedAt, want)
	}

	if _, ok := payload.Repos["beta-origin"]; ok {
		t.Fatal("NoEnvSync repo leaked into env.get_state")
	}
	if _, ok := payload.Repos["unknown-origin"]; ok {
		t.Fatal("unknown origin appeared in env.get_state response")
	}
}

// TestEnvGetStateOmitsTrackedFile proves a .env that was synced into the sidecar and
// then became git-tracked is not served: the shared LocalEnvState drop keeps a
// now-tracked name out of the response even though its sidecar entry survives.
func TestEnvGetStateOmitsTrackedFile(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	if err := os.MkdirAll(dl, 0o750); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	alpha := fx.JJClone(filepath.Join(dl, "alpha"))
	if err := os.WriteFile(filepath.Join(alpha, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	// Stage .env so vcs.TrackedNames reports it tracked.
	fx.RunGit(alpha, "add", ".env")

	initializeProductState(t)
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = dl
		s.AddRepo(state.Repo{Relpath: "alpha", Origin: fx.Origin, Trunk: "main"})
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// Seed a sidecar as if .env had been synced while still untracked — the leak was
	// Observe re-unioning that sidecar name back into the served state.
	configDir, err := state.Dir()
	if err != nil {
		t.Fatalf("config dir: %v", err)
	}
	seeded := cregistry.New[string]()
	seeded.Add("SECRET", "1", cregistry.UnixMicros(time.Now()))
	if err := (env.Sidecar{Origin: fx.Origin, Files: env.RepoState{".env": seeded}}).Save(env.SidecarPath(configDir, fx.Origin)); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	tx := servePipe(t)
	resp, err := tx.Do(t.Context(), &rpc.Request{Method: env.MethodGetState, Params: map[string]any{"origins": []any{fx.Origin}}})
	if err != nil {
		t.Fatalf("env.get_state: %v", err)
	}
	if !resp.OK {
		t.Fatalf("env.get_state not ok: %s", resp.Error)
	}
	var payload struct {
		Repos map[string]map[string]map[string]any `json:"repos"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Result)
	}
	if _, ok := payload.Repos[fx.Origin][".env"]; ok {
		t.Fatalf("git-tracked .env leaked into env.get_state: %s", resp.Result)
	}
}

// TestEnvGetStateRejectsTooManyOrigins proves a request asking for more origins than the
// cap is rejected before any state read.
func TestEnvGetStateRejectsTooManyOrigins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	origins := make([]any, 257)
	for i := range origins {
		origins[i] = fmt.Sprintf("origin-%d", i)
	}
	tx := servePipe(t)
	resp, err := tx.Do(t.Context(), &rpc.Request{Method: env.MethodGetState, Params: map[string]any{"origins": origins}})
	if err != nil {
		t.Fatalf("env.get_state: %v", err)
	}
	if resp.OK {
		t.Fatalf("env.get_state accepted %d origins, want rejection over the cap", len(origins))
	}
}

// TestEnvGetStateDedupesOrigins proves duplicate origins collapse: the response for a
// request repeating an origin is byte-identical to the deduped single-origin request.
func TestEnvGetStateDedupesOrigins(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	if err := os.MkdirAll(dl, 0o750); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	alpha := fx.JJClone(filepath.Join(dl, "alpha"))
	if err := os.WriteFile(filepath.Join(alpha, ".env"), []byte("API_KEY=secret\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	initializeProductState(t)
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = dl
		s.AddRepo(state.Repo{Relpath: "alpha", Origin: fx.Origin, Trunk: "main"})
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	tx := servePipe(t)
	dup, err := tx.Do(t.Context(), &rpc.Request{Method: env.MethodGetState, Params: map[string]any{"origins": []any{fx.Origin, fx.Origin, fx.Origin}}})
	if err != nil {
		t.Fatalf("dup request: %v", err)
	}
	deduped, err := tx.Do(t.Context(), &rpc.Request{Method: env.MethodGetState, Params: map[string]any{"origins": []any{fx.Origin}}})
	if err != nil {
		t.Fatalf("deduped request: %v", err)
	}
	if !dup.OK || !deduped.OK {
		t.Fatalf("env.get_state not ok: dup=%q deduped=%q", dup.Error, deduped.Error)
	}
	if string(dup.Result) != string(deduped.Result) {
		t.Fatalf("duplicate origins changed the response:\n dup = %s\n deduped = %s", dup.Result, deduped.Result)
	}
}
