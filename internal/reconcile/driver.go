package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/synckit/converge"
	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/state"
)

// peerStatus is the process-lived transition tracker converge.Reconcile logs against,
// so an unreachable peer warns once per outage rather than every pass. The resident
// rpc-serve child keeps it for its generation; each 15-minute synckitd reconcile tick
// is a fresh process, so a persistently-down peer emits one residual line per tick — by
// design, no state is persisted across processes.
var peerStatus = converge.NewPeerStatus()

// repoFetcher builds the peer registry fetcher Reconcile drives through convergeRepos;
// a var so a test can inject a fake without a real ssh hop.
var repoFetcher = func(runner syncservice.TransportRunner) converge.Fetcher[state.RepoMeta] {
	return sshFetcher{runner: runner}
}

// repoDriver implements synckit converge.Driver[state.RepoMeta] for the propagating
// (origin-keyed) repo registry: it reads and writes that registry inside reposync's
// state.json and clones-or-idle-syncs each present repo. It runs entirely inside the
// converge pass's flock, so its state reads and writes are lock-free.
type repoDriver struct {
	st      *state.State
	dl      string
	tmpRoot string
}

// LoadRegistry returns the propagating repo registry from the state the pass was
// handed — already a fresh read under the caller's lock, so it is not re-read here.
func (d *repoDriver) LoadRegistry(context.Context) (cregistry.Registry[state.RepoMeta], error) {
	return d.st.Repos, nil
}

// SaveRegistry writes the merged propagating registry back into state.json,
// foreign-key-preserving every other key, via the lock-free writer (the pass holds
// the flock). Local-only repos and the merged propagating set are both persisted.
func (d *repoDriver) SaveRegistry(_ context.Context, reg cregistry.Registry[state.RepoMeta]) error {
	d.st.Repos = reg
	return d.st.SaveReposUnlocked()
}

// Reconcile clones the repo if absent and idle-syncs it if present. The host-level
// anti-echo (skip the triggering peer) is handled by the converge fetch loop; a
// repo is reconciled onto disk the same regardless of which host triggered the pass,
// so the per-item origin is not consulted here.
func (d *repoDriver) Reconcile(ctx context.Context, id string, entry cregistry.Entry[state.RepoMeta], _ []string, _ string) (converge.Outcome, error) {
	res := reconcileOne(ctx, d.st, repoFor(id, entry), d.dl, d.tmpRoot)
	return converge.Outcome(res.Action), res.Err
}

// repoFor rebuilds the flat Repo view from a registry id (the origin) and its entry.
func repoFor(origin string, entry cregistry.Entry[state.RepoMeta]) state.Repo {
	return state.Repo{Relpath: entry.Value.Relpath, Origin: origin, Trunk: entry.Value.Trunk, LocalOnly: entry.Value.LocalOnly, NoEnvSync: entry.Value.NoEnvSync}
}

// sshFetcher reads a peer's propagating repo registry over the typed sync contract:
// it spawns `reposync rpc-serve` on the peer over ssh and calls svc.get-state —
// READ-ONLY, the pull side of pull-merge. It never writes to the peer. The transport
// self-heals while a peer's daemon is down, since rpc-serve reads state.json directly.
type sshFetcher struct{ runner syncservice.TransportRunner }

// Fetch returns peer's propagating repo registry, read over a one-shot ssh-stdio rpc
// session to the peer's rpc-serve bridge.
func (f sshFetcher) Fetch(ctx context.Context, peer string) (cregistry.Registry[state.RepoMeta], error) {
	c := syncservice.NewClient(f.runner.SSHStdio(peer, "reposync rpc-serve"))
	defer func() { _ = c.Close() }()
	raw, err := c.GetState(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch repo registry from %s: %w", peer, err)
	}
	reg, err := state.DecodeRepoRegistry(raw)
	if err != nil {
		return nil, fmt.Errorf("parse repo registry from %s: %w", peer, err)
	}
	return reg, nil
}

// convergeRepos runs one convergent-reconcile pass over the propagating registry:
// pull-merge every peer, persist the converged registry, then clone-or-sync each
// present repo. state.WithLock wraps the whole pass.
func convergeRepos(ctx context.Context, st *state.State, runner syncservice.TransportRunner, peers []string, origin string) ([]Result, error) {
	return convergeReposWith(ctx, st, repoFetcher(runner), peerStatus, peers, origin)
}

// convergeReposWith is convergeRepos with the peer fetcher and transition tracker
// injected so tests can drive the pull-merge against a mock peer, with a fresh tracker
// per test, without real ssh.
func convergeReposWith(ctx context.Context, st *state.State, f converge.Fetcher[state.RepoMeta], status *converge.PeerStatus, peers []string, origin string) ([]Result, error) {
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return nil, err
	}
	tmpRoot := filepath.Join(dl, TmpDirName)
	defer func() { _ = os.RemoveAll(tmpRoot) }()

	d := &repoDriver{st: st, dl: dl, tmpRoot: tmpRoot}
	items, err := converge.Reconcile(ctx, state.WithLock, d, f, status, peers, origin)
	if err != nil {
		return nil, err
	}
	return resultsFromItems(items, d.st.Repos), nil
}

// resultsFromItems maps the generic per-item results back to reposync's Result,
// resolving each item's id (its origin) to its relpath via the converged registry.
func resultsFromItems(items []converge.ItemResult, reg cregistry.Registry[state.RepoMeta]) []Result {
	out := make([]Result, len(items))
	for i, it := range items {
		out[i] = Result{Relpath: reg[it.ID].Value.Relpath, Action: string(it.Outcome), Err: it.Err}
	}
	return out
}
