package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/synckit/converge"
	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/hostregistry"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// fakeEnvFetcher serves a fixed per-peer, per-origin env state and records every peer it
// was asked for, so a test drives the pull-merge without real ssh. fail models an
// offline peer; unknown models an old-version peer that does not serve env.get_state.
type fakeEnvFetcher struct {
	states  map[string]map[string]env.RepoState
	fail    map[string]bool
	unknown map[string]bool
	called  []string
}

func (f *fakeEnvFetcher) FetchEnv(_ context.Context, peer string, origins []string) (map[string]env.RepoState, error) {
	f.called = append(f.called, peer)
	if f.fail[peer] {
		return nil, errors.New("offline")
	}
	if f.unknown[peer] {
		return nil, errUnknownMethod
	}
	out := map[string]env.RepoState{}
	for _, o := range origins {
		if rs, ok := f.states[peer][o]; ok {
			out[o] = rs
		}
	}
	return out, nil
}

// cloneRepo clones the harness origin into dataLoc/relpath as a colocated jj checkout —
// present on disk with a live git index for TrackedNames — and returns its abspath.
func (h *harness) cloneRepo(relpath string) string {
	h.t.Helper()
	dest := filepath.Join(h.dataLoc, relpath)
	if err := vcs.Clone(context.Background(), h.origin, dest); err != nil {
		h.t.Fatalf("clone %s: %v", relpath, err)
	}
	return dest
}

// writeEnvFile writes an env file under dir with its mtime pinned to at.
func writeEnvFile(t *testing.T, dir, name, content string, at time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

// backdated is an mtime well outside the quiet window, so a converge is never busy-gated.
func backdated() time.Time { return time.Now().Add(-time.Hour) }

func peerStamp() cregistry.Micros { return cregistry.UnixMicros(time.Now()) }

// oneKey builds a single-key present registry.
func oneKey(key, val string, at cregistry.Micros) env.FileMap {
	r := cregistry.New[string]()
	r.Add(key, val, at)
	return r
}

// sidecarPath returns the sidecar path for origin under the test's isolated config dir.
func sidecarPath(t *testing.T, origin string) string {
	t.Helper()
	dir, err := state.Dir()
	if err != nil {
		t.Fatalf("config dir: %v", err)
	}
	return env.SidecarPath(dir, origin)
}

func envState(peer, origin string, rs env.RepoState) map[string]map[string]env.RepoState {
	return map[string]map[string]env.RepoState{peer: {origin: rs}}
}

// TestConvergeEnvPeerKeyLands proves a peer-advertised key lands in both the local .env
// and the sidecar (env-applied), and a second pass is env-clean over a byte-identical file.
func TestConvergeEnvPeerKeyLands(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())})}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha env err: %v", res.Err)
	}
	if res.Action != ActionEnvApplied {
		t.Fatalf("action = %q, want env-applied", res.Action)
	}
	got := h.readFile(dest, ".env")
	if !strings.Contains(got, "API_KEY=secret") || !strings.Contains(got, "LOCAL=1") {
		t.Fatalf(".env = %q, want both LOCAL and API_KEY", got)
	}
	if !h.exists(sidecarPath(t, h.origin)) {
		t.Fatal("sidecar not created after env-applied")
	}

	// Second pass over the same peer: no change, byte-identical file.
	afterFirst := h.readFile(dest, ".env")
	second := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")
	res2 := resultFor(t, second, "alpha")
	if res2.Err != nil {
		t.Fatalf("second pass err: %v", res2.Err)
	}
	if res2.Action != ActionEnvClean {
		t.Fatalf("second action = %q, want env-clean", res2.Action)
	}
	if got := h.readFile(dest, ".env"); got != afterFirst {
		t.Fatalf("second pass rewrote the file:\n got: %q\nwant: %q", got, afterFirst)
	}
}

// TestConvergeEnvNewestWins proves the LWW conflict resolution both ways: a local edit
// newer than a peer's value wins, and a local edit older than a peer's value loses.
func TestConvergeEnvNewestWins(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	newer := time.Now().Add(-1 * time.Minute)

	t.Run("local newer beats peer older", func(t *testing.T) {
		h := newHarness(t)
		dest := h.cloneRepo("alpha")
		writeEnvFile(t, dest, ".env", "KEY=local\n", newer)
		st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

		f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("KEY", "peer", cregistry.UnixMicros(old))})}
		results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")

		if res := resultFor(t, results, "alpha"); res.Err != nil {
			t.Fatalf("err: %v", res.Err)
		}
		if got := h.readFile(dest, ".env"); !strings.Contains(got, "KEY=local") || strings.Contains(got, "KEY=peer") {
			t.Fatalf(".env = %q, want KEY=local to win", got)
		}
	})

	t.Run("local older loses to peer newer", func(t *testing.T) {
		h := newHarness(t)
		dest := h.cloneRepo("alpha")
		writeEnvFile(t, dest, ".env", "KEY=local\n", old)
		st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

		f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("KEY", "peer", cregistry.UnixMicros(newer))})}
		results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")

		if res := resultFor(t, results, "alpha"); res.Err != nil || res.Action != ActionEnvApplied {
			t.Fatalf("res = %+v, want env-applied no err", res)
		}
		if got := h.readFile(dest, ".env"); !strings.Contains(got, "KEY=peer") || strings.Contains(got, "KEY=local") {
			t.Fatalf(".env = %q, want KEY=peer to win", got)
		}
	})
}

// TestConvergeEnvDeletionPropagates proves a peer's tombstone removes the local line and
// the key does not resurrect on a re-pull.
func TestConvergeEnvDeletionPropagates(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	newer := time.Now().Add(-1 * time.Minute)

	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "KEY=1\nOTHER=keep\n", old)
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	peer := cregistry.New[string]()
	peer.Add("KEY", "1", cregistry.UnixMicros(old))
	peer.Remove("KEY", cregistry.UnixMicros(newer))
	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": peer})}

	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")
	if res := resultFor(t, results, "alpha"); res.Err != nil || res.Action != ActionEnvApplied {
		t.Fatalf("res = %+v, want env-applied no err", res)
	}
	got := h.readFile(dest, ".env")
	if strings.Contains(got, "KEY=1") {
		t.Fatalf(".env = %q, want KEY removed", got)
	}
	if !strings.Contains(got, "OTHER=keep") {
		t.Fatalf(".env = %q, want OTHER retained", got)
	}

	// Re-pull the same tombstone: KEY stays gone (not resurrected), clean pass.
	second := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")
	if res := resultFor(t, second, "alpha"); res.Err != nil || res.Action != ActionEnvClean {
		t.Fatalf("re-pull res = %+v, want env-clean no err", res)
	}
	if strings.Contains(h.readFile(dest, ".env"), "KEY=1") {
		t.Fatal("tombstoned KEY resurrected on re-pull")
	}
}

// TestConvergeEnvFetchesOriginPeer is the origin pin: env must NOT skip the notifying
// peer (opposite of the repo converge). Converging with origin == the sole peer must
// still fetch it and land its key.
func TestConvergeEnvFetchesOriginPeer(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{states: envState("hostA", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())})}
	convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"hostA"}, "hostA")

	if len(f.called) != 1 || f.called[0] != "hostA" {
		t.Fatalf("origin peer not fetched: called = %v (env must not skip the notifying origin)", f.called)
	}
	if got := h.readFile(dest, ".env"); !strings.Contains(got, "API_KEY=secret") {
		t.Fatalf(".env = %q, want the origin peer's key applied", got)
	}
}

// TestConvergeEnvOfflinePeerSelfHeals proves one unreachable peer does not abort the
// repo: a reachable peer's key still lands and there is no Result error.
func TestConvergeEnvOfflinePeerSelfHeals(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{
		states: envState("up", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())}),
		fail:   map[string]bool{"down": true},
	}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"down", "up"}, "")

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("offline peer aborted the repo: %v", res.Err)
	}
	if got := h.readFile(dest, ".env"); !strings.Contains(got, "API_KEY=secret") {
		t.Fatalf(".env = %q, want the reachable peer's key applied", got)
	}
}

// TestConvergeEnvOldVersionPeerSkips proves an old-version peer (unknown-method error)
// skips cleanly: no Result error, the repo still converges its local state.
func TestConvergeEnvOldVersionPeerSkips(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{unknown: map[string]bool{"old": true}}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"old"}, "")

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("old-version peer errored the repo: %v", res.Err)
	}
	if got := h.readFile(dest, ".env"); got != "LOCAL=1\n" {
		t.Fatalf(".env = %q, want untouched local state", got)
	}
}

// TestConvergeEnvTrackedFileUntouched proves a git-tracked .env is neither observed,
// overwritten, nor persisted to a sidecar, even when a peer advertises a different value.
func TestConvergeEnvTrackedFileUntouched(t *testing.T) {
	h := newHarness(t)
	h.writeFile(h.seed, ".env", "COMMITTED=1\n")
	h.runGit(h.seed, "add", ".env")
	h.runGit(h.seed, "commit", "-qm", "add env")
	h.runGit(h.seed, "push", "-q", "origin", "main")
	dest := h.cloneRepo("alpha")
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())})}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if res.Action != ActionEnvClean {
		t.Fatalf("action = %q, want env-clean (tracked file dropped)", res.Action)
	}
	if got := h.readFile(dest, ".env"); got != "COMMITTED=1\n" {
		t.Fatalf("tracked .env = %q, want untouched COMMITTED=1", got)
	}
	if h.exists(sidecarPath(t, h.origin)) {
		t.Fatal("sidecar created for a git-tracked .env")
	}
}

// TestConvergeEnvSkipsOptOutAndLocalOnly proves a NoEnvSync repo and a local-only repo
// are ineligible: neither is converged, no sidecar is created, and their files are untouched.
func TestConvergeEnvSkipsOptOutAndLocalOnly(t *testing.T) {
	h := newHarness(t)
	noEnv := h.cloneRepo("noenv")
	writeEnvFile(t, noEnv, ".env", "SECRET=1\n", backdated())
	localOnly := h.cloneRepo("localonly")
	writeEnvFile(t, localOnly, ".env", "SECRET=2\n", backdated())

	st := h.state(
		state.Repo{Relpath: "noenv", Origin: h.origin, Trunk: "main", NoEnvSync: true},
		state.Repo{Relpath: "localonly", Trunk: "main", LocalOnly: true},
	)

	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())})}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")

	if len(results) != 0 {
		t.Fatalf("results = %+v, want none (both repos ineligible)", results)
	}
	if len(f.called) != 0 {
		t.Fatalf("fetched peers %v, want none when no repo is eligible", f.called)
	}
	if got := h.readFile(noEnv, ".env"); got != "SECRET=1\n" {
		t.Fatalf("no-env-sync .env = %q, want untouched", got)
	}
	if got := h.readFile(localOnly, ".env"); got != "SECRET=2\n" {
		t.Fatalf("local-only .env = %q, want untouched", got)
	}
	if h.exists(sidecarPath(t, h.origin)) {
		t.Fatal("sidecar created for an ineligible repo")
	}
}

// TestConvergeEnvQuietWindow proves a file modified within the quiet window gates the
// repo env-busy and nothing is persisted: no sidecar, no file change.
func TestConvergeEnvQuietWindow(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", time.Now())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())})}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, "")

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if res.Action != ActionEnvBusy {
		t.Fatalf("action = %q, want env-busy", res.Action)
	}
	if h.exists(sidecarPath(t, h.origin)) {
		t.Fatal("sidecar persisted despite env-busy")
	}
	if got := h.readFile(dest, ".env"); got != "LOCAL=1\n" {
		t.Fatalf("busy .env = %q, want untouched", got)
	}
}

// TestConvergeEnvBootstrapTwoHosts proves two hosts with pre-existing divergent .env
// files and no sidecars converge both ways to byte-level KV equality and an equal digest,
// with the newer edit winning the shared key.
func TestConvergeEnvBootstrapTwoHosts(t *testing.T) {
	h := newHarness(t)
	older := time.Now().Add(-20 * time.Minute)
	nwer := time.Now().Add(-10 * time.Minute)

	dataA := filepath.Join(h.root, "dataA")
	dataB := filepath.Join(h.root, "dataB")
	for _, d := range []string{dataA, dataB} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	destA := filepath.Join(dataA, "alpha")
	destB := filepath.Join(dataB, "alpha")
	if err := vcs.Clone(context.Background(), h.origin, destA); err != nil {
		t.Fatalf("clone A: %v", err)
	}
	if err := vcs.Clone(context.Background(), h.origin, destB); err != nil {
		t.Fatalf("clone B: %v", err)
	}
	writeEnvFile(t, destA, ".env", "A_ONLY=1\nSHARED=fromA\n", nwer)
	writeEnvFile(t, destB, ".env", "B_ONLY=2\nSHARED=fromB\n", older)

	xdgA := filepath.Join(h.root, "xdgA")
	xdgB := filepath.Join(h.root, "xdgB")

	hostState := func(dl string) *state.State {
		st := state.New()
		st.DefaultLocation = dl
		st.Settings = state.Settings{IdleThreshold: state.Duration(time.Nanosecond), RepoOpTimeout: state.Duration(time.Minute)}
		st.AddRepo(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
		return st
	}
	stA := hostState(dataA)
	stB := hostState(dataB)

	// serve computes what the peer at peerXDG/peerDest would advertise: its real Observe
	// output against its own sidecar.
	serve := func(peerXDG, peerDest string) *fakeEnvFetcher {
		sc, err := env.LoadSidecar(env.SidecarPath(filepath.Join(peerXDG, "reposync"), h.origin), h.origin)
		if err != nil {
			t.Fatalf("load peer sidecar: %v", err)
		}
		rs, err := env.Observe(sc, peerDest, []string{".env"})
		if err != nil {
			t.Fatalf("observe peer: %v", err)
		}
		return &fakeEnvFetcher{states: envState("peer", h.origin, rs)}
	}

	for round := 0; round < 4; round++ {
		t.Setenv("XDG_CONFIG_HOME", xdgA)
		convergeEnvWith(context.Background(), stA, serve(xdgB, destB), converge.NewPeerStatus(), []string{"peer"}, "")
		t.Setenv("XDG_CONFIG_HOME", xdgB)
		convergeEnvWith(context.Background(), stB, serve(xdgA, destA), converge.NewPeerStatus(), []string{"peer"}, "")
	}

	gotA := h.readFile(destA, ".env")
	gotB := h.readFile(destB, ".env")
	for _, want := range []string{"A_ONLY=1", "B_ONLY=2", "SHARED=fromA"} {
		if !strings.Contains(gotA, want) {
			t.Fatalf("host A .env = %q, want %q", gotA, want)
		}
		if !strings.Contains(gotB, want) {
			t.Fatalf("host B .env = %q, want %q", gotB, want)
		}
	}
	if strings.Contains(gotA, "SHARED=fromB") || strings.Contains(gotB, "SHARED=fromB") {
		t.Fatalf("stale SHARED=fromB survived: A=%q B=%q", gotA, gotB)
	}

	digestOf := func(dest string) string {
		rs, err := env.Observe(env.Sidecar{Origin: h.origin, Files: env.RepoState{}}, dest, []string{".env"})
		if err != nil {
			t.Fatalf("observe for digest: %v", err)
		}
		return env.Digest(rs)
	}
	if da, db := digestOf(destA), digestOf(destB); da != db {
		t.Fatalf("digests diverged after convergence: A=%s B=%s", da, db)
	}
}

// TestReconcileFreshCloneMaterializesEnv proves a full Reconcile clones a peer-advertised
// repo AND materializes the peer's env file in the same pass.
func TestReconcileFreshCloneMaterializesEnv(t *testing.T) {
	h := newHarness(t)
	seedMesh(t, "yasyf@self", "yasyf@peer")

	oldRepo := repoFetcher
	repoFetcher = newPeerFetcher(peerRegistry(h.origin, state.RepoMeta{Relpath: "alpha", Trunk: "main"}, 100))
	t.Cleanup(func() { repoFetcher = oldRepo })

	oldEnv := envFetch
	envFetch = &fakeEnvFetcher{states: envState("yasyf@peer", h.origin, env.RepoState{".env": oneKey("API_KEY", "secret", peerStamp())})}
	t.Cleanup(func() { envFetch = oldEnv })

	st := h.state() // local host tracks nothing; the peer advertises the repo
	results, err := Reconcile(context.Background(), st, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) {
		t.Fatal("peer-advertised repo was not cloned")
	}
	if got := h.readFile(dest, ".env"); !strings.Contains(got, "API_KEY=secret") {
		t.Fatalf(".env not materialized in the same pass: %q", got)
	}
	for _, r := range results {
		if r.Relpath == "alpha" && r.Err != nil {
			t.Fatalf("alpha result carried an error: %+v", r)
		}
	}
}

// TestConvergeEnvRejectsMaliciousPeers proves a peer serving a bad file name, a bad key,
// or a newline-injecting value has its ENTIRE payload rejected, none of it reaching disk
// or the sidecar, while a well-formed peer's payload still applies.
func TestConvergeEnvRejectsMaliciousPeers(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{states: map[string]map[string]env.RepoState{
		"traversal": {h.origin: env.RepoState{"../evil": oneKey("X", "1", peerStamp())}},
		"dotfile":   {h.origin: env.RepoState{".bashrc": oneKey("X", "1", peerStamp())}},
		"badkey":    {h.origin: env.RepoState{".env": oneKey("FOO\nBAR", "1", peerStamp())}},
		"badvalue":  {h.origin: env.RepoState{".env": oneKey("FOO", "line1\nline2", peerStamp())}},
		"good":      {h.origin: env.RepoState{".env": oneKey("GOOD", "yes", peerStamp())}},
	}}
	peers := []string{"traversal", "dotfile", "badkey", "badvalue", "good"}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), peers, "")

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("malicious peers errored the repo: %v", res.Err)
	}
	got := h.readFile(dest, ".env")
	if !strings.Contains(got, "GOOD=yes") {
		t.Fatalf(".env = %q, want the good peer's key applied", got)
	}
	for _, bad := range []string{"evil", "BAR", "line2", ".bashrc"} {
		if strings.Contains(got, bad) {
			t.Fatalf(".env = %q, leaked malicious content %q", got, bad)
		}
	}
	if h.exists(filepath.Join(h.dataLoc, "evil")) || h.exists(filepath.Join(dest, ".bashrc")) {
		t.Fatal("a malicious file name was written to disk")
	}
}

// TestConvergeEnvRejectsFutureStamp proves a peer serving an entry stamped past
// MaxStampSkew has its ENTIRE payload rejected while a well-formed peer still applies.
func TestConvergeEnvRejectsFutureStamp(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	future := cregistry.UnixMicros(time.Now().Add(env.MaxStampSkew + time.Hour))
	f := &fakeEnvFetcher{states: map[string]map[string]env.RepoState{
		"future": {h.origin: env.RepoState{".env": oneKey("EVIL", "x", future)}},
		"good":   {h.origin: env.RepoState{".env": oneKey("GOOD", "yes", peerStamp())}},
	}}
	results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"future", "good"}, "")

	if res := resultFor(t, results, "alpha"); res.Err != nil {
		t.Fatalf("future-stamp peer errored the repo: %v", res.Err)
	}
	got := h.readFile(dest, ".env")
	if !strings.Contains(got, "GOOD=yes") {
		t.Fatalf(".env = %q, want the good peer's key applied", got)
	}
	if strings.Contains(got, "EVIL") {
		t.Fatalf(".env = %q, leaked a future-stamped key", got)
	}
}

// TestConvergeEnvRejectsOversizedPayloads proves a peer payload over any wire cap —
// files per origin, keys per file, or per-file aggregate bytes — is rejected whole
// while a well-formed peer still applies its key.
func TestConvergeEnvRejectsOversizedPayloads(t *testing.T) {
	manyFiles := env.RepoState{}
	for i := 0; i <= env.MaxWireFiles; i++ {
		manyFiles[fmt.Sprintf(".env.f%d", i)] = oneKey("K", "v", peerStamp())
	}
	manyKeys := cregistry.New[string]()
	for i := 0; i <= env.MaxWireKeys; i++ {
		manyKeys.Add(fmt.Sprintf("K%d", i), "v", peerStamp())
	}
	bigValue := cregistry.New[string]()
	bigValue.Add("K", strings.Repeat("x", env.MaxFileSize), peerStamp())

	cases := []struct {
		id  string
		bad env.RepoState
	}{
		{"too many files", manyFiles},
		{"too many keys", env.RepoState{".env": manyKeys}},
		{"aggregate over MaxFileSize", env.RepoState{".env": bigValue}},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			h := newHarness(t)
			dest := h.cloneRepo("alpha")
			writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
			st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

			f := &fakeEnvFetcher{states: map[string]map[string]env.RepoState{
				"bad":  {h.origin: c.bad},
				"good": {h.origin: env.RepoState{".env": oneKey("GOOD", "yes", peerStamp())}},
			}}
			results := convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"bad", "good"}, "")

			if res := resultFor(t, results, "alpha"); res.Err != nil {
				t.Fatalf("oversized peer errored the repo: %v", res.Err)
			}
			if got := h.readFile(dest, ".env"); got != "LOCAL=1\nGOOD=yes\n" {
				t.Fatalf(".env = %q, want only LOCAL and the good peer's GOOD (bad payload rejected whole)", got)
			}
		})
	}
}

// TestConvergeEnvDropsNeverLocalTombstone proves a peer's tombstone-only entry for a
// name this host has never held materializes nothing and is never persisted, so the
// sidecar cannot accumulate tombstone spam under never-existing names.
func TestConvergeEnvDropsNeverLocalTombstone(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	newer := time.Now().Add(-time.Minute)

	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "LOCAL=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	ghost := cregistry.New[string]()
	ghost.Add("SECRET", "was-here", cregistry.UnixMicros(old))
	ghost.Remove("SECRET", cregistry.UnixMicros(newer))
	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{
		".env":       oneKey("GOOD", "yes", peerStamp()),
		".env.ghost": ghost,
	})}

	if res := resultFor(t, convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, ""), "alpha"); res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if h.exists(filepath.Join(dest, ".env.ghost")) {
		t.Fatal(".env.ghost materialized from a tombstone-only peer entry")
	}
	sc, err := env.LoadSidecar(sidecarPath(t, h.origin), h.origin)
	if err != nil {
		t.Fatalf("load sidecar: %v", err)
	}
	if _, ok := sc.Files[".env.ghost"]; ok {
		t.Fatal(".env.ghost tombstone persisted to the sidecar, want dropped")
	}
	if _, ok := sc.Files[".env"]; !ok {
		t.Fatal(".env not persisted, want the synced file remembered")
	}
}

// TestConvergeEnvWholeFileDeletionPropagates proves a locally deleted synced file keeps
// its tombstoned (blank-valued) name in the deleting host's sidecar — never pruned as
// tombstone-only, since the host held it — and a second host pulling that host's served
// state applies the deletion.
func TestConvergeEnvWholeFileDeletionPropagates(t *testing.T) {
	h := newHarness(t)

	dataA := filepath.Join(h.root, "dataA")
	dataB := filepath.Join(h.root, "dataB")
	for _, d := range []string{dataA, dataB} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	destA := filepath.Join(dataA, "alpha")
	destB := filepath.Join(dataB, "alpha")
	if err := vcs.Clone(context.Background(), h.origin, destA); err != nil {
		t.Fatalf("clone A: %v", err)
	}
	if err := vcs.Clone(context.Background(), h.origin, destB); err != nil {
		t.Fatalf("clone B: %v", err)
	}
	writeEnvFile(t, destA, ".env.extra", "SECRET=1\n", backdated())
	writeEnvFile(t, destB, ".env.extra", "SECRET=1\n", backdated())

	xdgA := filepath.Join(h.root, "xdgA")
	xdgB := filepath.Join(h.root, "xdgB")
	hostState := func(dl string) *state.State {
		st := state.New()
		st.DefaultLocation = dl
		st.Settings = state.Settings{IdleThreshold: state.Duration(time.Nanosecond), RepoOpTimeout: state.Duration(time.Minute)}
		st.AddRepo(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
		return st
	}
	stA, stB := hostState(dataA), hostState(dataB)
	sidecarA := env.SidecarPath(filepath.Join(xdgA, "reposync"), h.origin)

	// Sync .env.extra into A's sidecar, then delete it locally and converge again.
	t.Setenv("XDG_CONFIG_HOME", xdgA)
	if res := resultFor(t, convergeEnvWith(context.Background(), stA, &fakeEnvFetcher{}, converge.NewPeerStatus(), nil, ""), "alpha"); res.Err != nil {
		t.Fatalf("seed converge on A: %v", res.Err)
	}
	if err := os.Remove(filepath.Join(destA, ".env.extra")); err != nil {
		t.Fatalf("delete .env.extra: %v", err)
	}
	if res := resultFor(t, convergeEnvWith(context.Background(), stA, &fakeEnvFetcher{}, converge.NewPeerStatus(), nil, ""), "alpha"); res.Err != nil {
		t.Fatalf("post-delete converge on A: %v", res.Err)
	}

	scA, err := env.LoadSidecar(sidecarA, h.origin)
	if err != nil {
		t.Fatalf("load A sidecar: %v", err)
	}
	reg, ok := scA.Files[".env.extra"]
	if !ok {
		t.Fatal("deleted .env.extra pruned from A's sidecar, want tombstones retained to propagate the deletion")
	}
	if e := reg["SECRET"]; e.Present() || e.Value != "" {
		t.Fatalf("SECRET = %+v, want a blank-valued tombstone", e)
	}

	// B pulls A's real served state and applies the deletion.
	served, err := LocalEnvState(context.Background(), destA, sidecarA, h.origin)
	if err != nil {
		t.Fatalf("LocalEnvState on A: %v", err)
	}
	if _, ok := served[".env.extra"]; !ok {
		t.Fatal("A stopped serving the deleted file's tombstones")
	}
	t.Setenv("XDG_CONFIG_HOME", xdgB)
	f := &fakeEnvFetcher{states: envState("hostA", h.origin, served)}
	if res := resultFor(t, convergeEnvWith(context.Background(), stB, f, converge.NewPeerStatus(), []string{"hostA"}, ""), "alpha"); res.Err != nil || res.Action != ActionEnvApplied {
		t.Fatalf("converge on B = %+v, want env-applied", res)
	}
	if h.exists(filepath.Join(destB, ".env.extra")) {
		if got := h.readFile(destB, ".env.extra"); strings.Contains(got, "SECRET") {
			t.Fatalf("B .env.extra = %q, want SECRET removed", got)
		}
	}
}

// TestConvergeEnvTrackedAfterSyncPurges proves a synced .env that later becomes
// git-tracked stops being served by LocalEnvState AND is purged from the persisted
// sidecar on the next converge, even while a peer keeps advertising it.
func TestConvergeEnvTrackedAfterSyncPurges(t *testing.T) {
	h := newHarness(t)
	dest := h.cloneRepo("alpha")
	writeEnvFile(t, dest, ".env", "SECRET=1\n", backdated())
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})

	f := &fakeEnvFetcher{states: envState("peer", h.origin, env.RepoState{".env": oneKey("SECRET", "1", peerStamp())})}
	if res := resultFor(t, convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, ""), "alpha"); res.Err != nil {
		t.Fatalf("seed converge: %v", res.Err)
	}
	if sc, err := env.LoadSidecar(sidecarPath(t, h.origin), h.origin); err != nil {
		t.Fatalf("load sidecar: %v", err)
	} else if _, ok := sc.Files[".env"]; !ok {
		t.Fatal("precondition: .env not synced into the sidecar")
	}

	// Stage .env in git so vcs.TrackedNames reports it tracked from now on.
	h.runGit(dest, "add", ".env")

	served, err := LocalEnvState(context.Background(), dest, sidecarPath(t, h.origin), h.origin)
	if err != nil {
		t.Fatalf("LocalEnvState: %v", err)
	}
	if _, ok := served[".env"]; ok {
		t.Fatal("now-tracked .env still served by LocalEnvState")
	}

	if res := resultFor(t, convergeEnvWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"peer"}, ""), "alpha"); res.Err != nil {
		t.Fatalf("second converge: %v", res.Err)
	}
	sc, err := env.LoadSidecar(sidecarPath(t, h.origin), h.origin)
	if err != nil {
		t.Fatalf("reload sidecar: %v", err)
	}
	if _, ok := sc.Files[".env"]; ok {
		t.Fatal("now-tracked .env survived in the persisted sidecar, want purged")
	}
	if got := h.readFile(dest, ".env"); got != "SECRET=1\n" {
		t.Fatalf("tracked .env = %q, want untouched", got)
	}
}

// seedMesh writes a self+hosts identity into the shared synckit mesh so a full Reconcile
// sees peers.
func seedMesh(t *testing.T, self string, hosts ...string) {
	t.Helper()
	if _, err := hostregistry.Mesh.Update(context.Background(), func(g *hostregistry.Registry) error {
		g.Self = self
		for _, h := range hosts {
			g.UpsertHost(h)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed mesh: %v", err)
	}
}
