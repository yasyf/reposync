package reconcile

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yasyf/synckit/converge"
	"github.com/yasyf/synckit/cregistry"

	"github.com/yasyf/reposync/internal/state"
)

// peerFetcher is a fake converge.Fetcher that serves a fixed propagating registry for
// every reachable peer and records each peer it was asked for, so a test can assert
// the pull-merge was read-only without any real ssh. A peer in fail errors, modeling
// an offline host while another answers.
type peerFetcher struct {
	reg    cregistry.Registry[state.RepoMeta]
	fail   map[string]bool
	called []string
}

func newPeerFetcher(reg cregistry.Registry[state.RepoMeta]) *peerFetcher {
	return &peerFetcher{reg: reg, fail: map[string]bool{}}
}

func (f *peerFetcher) Fetch(_ context.Context, peer string) (cregistry.Registry[state.RepoMeta], error) {
	f.called = append(f.called, peer)
	if f.fail[peer] {
		return nil, context.DeadlineExceeded
	}
	return f.reg, nil
}

// peerRegistry builds the propagating registry a peer's rpc-serve get-state would emit
// for a single repo entry, the registry the fake fetcher serves to the converge pass.
func peerRegistry(origin string, meta state.RepoMeta, added cregistry.Micros) cregistry.Registry[state.RepoMeta] {
	reg := cregistry.New[state.RepoMeta]()
	reg.Add(origin, meta, added)
	return reg
}

// TestConvergeClonesPeerAdvertisedRepo proves pull-merge: a repo this host does not
// track, advertised present by a peer over get-state, is merged into the local
// registry and cloned onto disk — no peer push, the peer's registry is read only.
func TestConvergeClonesPeerAdvertisedRepo(t *testing.T) {
	h := newHarness(t)
	st := h.state() // local host tracks nothing

	f := newPeerFetcher(peerRegistry(h.origin, state.RepoMeta{Relpath: "alpha", Trunk: "main"}, 100))

	results, err := convergeReposWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"yasyf@peer"}, "")
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
	// The fetch was read-only: the only peer interaction is the get-state read.
	if len(f.called) != 1 || f.called[0] != "yasyf@peer" {
		t.Fatalf("pull-merge fetched peers %v; convergence must read each peer once", f.called)
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
	f := newPeerFetcher(peer)

	results, err := convergeReposWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"yasyf@peer"}, "")
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

	// up@peer answers get-state; down@peer errors on every fetch, modeling an
	// offline host.
	f := newPeerFetcher(peerRegistry(h.origin, state.RepoMeta{Relpath: "alpha", Trunk: "main"}, 100))
	f.fail["down@peer"] = true

	results, err := convergeReposWith(context.Background(), st, f, converge.NewPeerStatus(), []string{"down@peer", "up@peer"}, "")
	if err != nil {
		t.Fatalf("an offline peer must not abort the pass: %v", err)
	}
	if res := resultFor(t, results, "alpha"); res.Err != nil || res.Action != ActionCloned {
		t.Fatalf("reachable peer's repo did not converge: %+v", res)
	}
}
