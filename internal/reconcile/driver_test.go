package reconcile

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/hostregistry"

	"github.com/yasyf/reposync/internal/state"
)

// peerRegistryJSON builds the JSON a peer's `state get-json` would emit for a single
// repo registry entry, so a mock runner can serve it to the Fetcher.
func peerRegistryJSON(t *testing.T, reg cregistry.Registry[state.RepoMeta]) string {
	t.Helper()
	st := state.New()
	st.Repos = reg
	data, err := st.EncodeRepoRegistry()
	if err != nil {
		t.Fatalf("encode peer registry: %v", err)
	}
	return string(data)
}

// TestConvergeClonesPeerAdvertisedRepo proves pull-merge: a repo this host does not
// track, advertised present by a peer over `state get-json`, is merged into the local
// registry and cloned onto disk — no peer push, the peer's registry is read only.
func TestConvergeClonesPeerAdvertisedRepo(t *testing.T) {
	h := newHarness(t)
	st := h.state() // local host tracks nothing

	peer := cregistry.New[state.RepoMeta]()
	peer.Add(h.origin, state.RepoMeta{Relpath: "alpha", Trunk: "main"}, 100)
	runner := hostregistry.NewMockRunner().OnSSH(GetJSONCmd, peerRegistryJSON(t, peer), nil)

	results, err := convergeReposWith(context.Background(), st, runner, []string{"yasyf@peer"}, "")
	if err != nil {
		t.Fatalf("convergeRepos: %v", err)
	}

	res := resultFor(t, results, "alpha")
	if res.Err != nil {
		t.Fatalf("alpha reconcile err: %v", res.Err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("alpha action = %q, want cloned", res.Action)
	}
	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) {
		t.Fatal("peer-advertised repo was not cloned onto disk")
	}
	// The merge persisted the peer's entry into this host's registry.
	if e, ok := st.Repos[h.origin]; !ok || !e.Present() || e.Value.Relpath != "alpha" {
		t.Fatalf("peer repo not merged into local registry: %v", st.Repos)
	}
	// The fetch was read-only: the only ssh call is the get-json read.
	for _, c := range runner.SSHCmdsAll() {
		if c != GetJSONCmd {
			t.Fatalf("pull-merge made a non-read ssh call %q; convergence must be pull-only", c)
		}
	}
}

// TestConvergeTombstonePropagates proves a peer's removal converges here: a repo this
// host tracks-and-has-cloned, advertised by a peer as tombstoned (removed_at later
// than added_at), converges to absent locally — its registry entry is tombstoned and
// it is no longer reconciled. The on-disk checkout is left in place (untrack is not
// delete).
func TestConvergeTombstonePropagates(t *testing.T) {
	h := newHarness(t)
	// Track and clone the repo locally first.
	st := h.state(state.Repo{Relpath: "alpha", Origin: h.origin, Trunk: "main"})
	if _, err := Reconcile(context.Background(), st, ""); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}
	dest := filepath.Join(h.dataLoc, "alpha")
	if !h.exists(filepath.Join(dest, ".jj")) {
		t.Fatal("precondition: alpha not cloned before tombstone")
	}
	localAdd := st.Repos[h.origin].Added

	// Peer advertises the same repo tombstoned at a strictly-later stamp.
	peer := cregistry.New[state.RepoMeta]()
	peer.Add(h.origin, state.RepoMeta{Relpath: "alpha", Trunk: "main"}, localAdd)
	peer.Remove(h.origin, localAdd+1)
	runner := hostregistry.NewMockRunner().OnSSH(GetJSONCmd, peerRegistryJSON(t, peer), nil)

	results, err := convergeReposWith(context.Background(), st, runner, []string{"yasyf@peer"}, "")
	if err != nil {
		t.Fatalf("convergeRepos: %v", err)
	}

	// The tombstone converged: the entry is absent, so it is not reconciled.
	for _, r := range results {
		if r.Relpath == "alpha" {
			t.Fatalf("tombstoned repo was still reconciled: %+v", r)
		}
	}
	entry, ok := st.Repos[h.origin]
	if !ok {
		t.Fatal("tombstoned entry dropped from registry: removal would not propagate onward")
	}
	if entry.Present() {
		t.Fatal("repo still present after a peer tombstone converged")
	}
	// The checkout is untouched — untrack keeps the working copy.
	if !h.exists(filepath.Join(dest, ".jj")) {
		t.Fatal("tombstone deleted the on-disk checkout")
	}
}

// TestConvergeOfflinePeerSelfHeals proves one unreachable peer does not abort the
// pass: a reachable peer's repo still converges and clones while the offline peer's
// fetch error is skipped.
func TestConvergeOfflinePeerSelfHeals(t *testing.T) {
	h := newHarness(t)
	st := h.state()

	peer := cregistry.New[state.RepoMeta]()
	peer.Add(h.origin, state.RepoMeta{Relpath: "alpha", Trunk: "main"}, 100)
	// up@peer answers get-json; down@peer errors on every ssh (unscripted, no default).
	runner := hostregistry.NewMockRunner().OnSSH(GetJSONCmd, peerRegistryJSON(t, peer), nil)
	runner2 := &targetFailingFetcher{MockRunner: runner, failTarget: "down@peer"}

	results, err := convergeReposWith(context.Background(), st, runner2, []string{"down@peer", "up@peer"}, "")
	if err != nil {
		t.Fatalf("an offline peer must not abort the pass: %v", err)
	}
	if res := resultFor(t, results, "alpha"); res.Err != nil || res.Action != ActionCloned {
		t.Fatalf("reachable peer's repo did not converge: %+v", res)
	}
}

// targetFailingFetcher forces ssh to one target to fail, modeling an offline peer
// while another answers.
type targetFailingFetcher struct {
	*hostregistry.MockRunner
	failTarget string
}

func (f *targetFailingFetcher) SSH(ctx context.Context, target, cmd string) (string, error) {
	if target == f.failTarget {
		return "", context.DeadlineExceeded
	}
	return f.MockRunner.SSH(ctx, target, cmd)
}
