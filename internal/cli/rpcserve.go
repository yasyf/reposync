package cli

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/synckit/rpc"
	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

func newRPCServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rpc-serve",
		Short: "Serve the typed sync contract over stdio for synckitd.",
		Long: "Run a long-lived request/response loop on stdin/stdout, serving synckitd's " +
			"typed sync methods (capabilities, list, reconcile, sync, get-state) until the " +
			"pipe closes. synckitd starts this on demand — locally over a spawned child and " +
			"on a peer over ssh — so reconcile/list/get-state are never shelled as separate " +
			"commands. Stdout carries the rpc frames; all diagnostics go to stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// stdout is the rpc framing channel: route the std logger to stderr so a
			// stray log line never corrupts a response frame.
			log.SetOutput(os.Stderr)

			return rpc.NewServer(newServeDispatcher()).ServeSession(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
	return cmd
}

// newServeDispatcher builds the dispatcher `reposync rpc-serve` answers: the typed
// synckit sync contract plus reposync's env.get_state env-state read.
func newServeDispatcher() *rpc.Dispatcher {
	d := rpc.NewDispatcher()
	syncservice.RegisterConsumer(d, repoConsumer{})
	d.Register(env.MethodGetState, envGetState)
	return d
}

// envStateResult is the env.get_state response envelope: each served origin to its
// observed RepoState (a cregistry Entry map per file).
type envStateResult struct {
	Repos map[string]env.RepoState `json:"repos"`
}

// envGetState serves a peer's request for this host's stamped env state: for each
// requested origin that maps to a tracked, present, env-syncing repo, the untracked root
// .env files observed against their sidecar. It is strictly read-only and takes NO flock
// — every peer's converge holds its own flock across this fetch, so a locking handler
// would deadlock the mesh (the same reason GetState above stays lock-free).
func envGetState(ctx context.Context, p map[string]any) (any, error) {
	origins, err := originsParam(p)
	if err != nil {
		return nil, err
	}
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return nil, err
	}
	configDir, err := state.Dir()
	if err != nil {
		return nil, err
	}
	repos := make(map[string]env.RepoState)
	for _, origin := range origins {
		rs, ok, err := servedEnvState(ctx, st, dl, configDir, origin)
		if err != nil {
			return nil, err
		}
		if ok {
			repos[origin] = rs
		}
	}
	return envStateResult{Repos: repos}, nil
}

// servedEnvState returns origin's syncable env state when it is a tracked, present-on-disk,
// env-syncing repo; ok is false for an unknown, absent, or ineligible origin, which the
// caller skips rather than errors. It shares reconcile.LocalEnvState with the converge
// pass, so a now-git-tracked, exempt, or invalid name is never served.
func servedEnvState(ctx context.Context, st *state.State, dl, configDir, origin string) (env.RepoState, bool, error) {
	repo, ok := st.FindRepoByOrigin(origin)
	if !ok || repo.LocalOnly || repo.NoEnvSync {
		return nil, false, nil
	}
	abspath := repo.AbsPath(dl)
	if !reconcile.Present(abspath) {
		return nil, false, nil
	}
	rs, err := reconcile.LocalEnvState(ctx, abspath, env.SidecarPath(configDir, origin), origin)
	if err != nil {
		return nil, false, err
	}
	return rs, true, nil
}

// originsParam extracts the requested origins from the request params, rejecting a
// non-string entry or an over-cap count so a malformed or oversized request fails loudly
// rather than serving garbage. Duplicates are collapsed so each origin costs one pass.
func originsParam(p map[string]any) ([]string, error) {
	raw, ok := p["origins"]
	if !ok {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("env.get_state: origins must be an array of strings")
	}
	if len(arr) > env.MaxOrigins {
		return nil, fmt.Errorf("env.get_state: %d origins over the %d limit", len(arr), env.MaxOrigins)
	}
	seen := make(map[string]bool, len(arr))
	out := make([]string, 0, len(arr))
	for i, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("env.get_state: origins[%d] is not a string", i)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// repoConsumer is reposync's [syncservice.SyncConsumer]: it serves synckitd's typed
// sync contract straight off the on-disk state. Each method loads state fresh —
// state.Load is a cheap file read and the rpc dispatcher serializes every request, so
// a per-request read never races. It is PULL-ONLY across hosts: get-state exposes the
// propagating (origin-keyed) registry alone, never the local-only repos.
type repoConsumer struct{}

// Capabilities reports reposync's name, the protocol version it speaks, and its
// methods.
func (repoConsumer) Capabilities(_ context.Context) (syncservice.Capabilities, error) {
	return syncservice.DefaultCapabilities(state.ToolName), nil
}

// List enumerates every tracked repo as a watch item: propagating repos keyed by
// origin and local-only repos keyed by relpath, each with the VCS metadata
// directories to watch and the upstream trunk hash as its change fingerprint.
func (repoConsumer) List(ctx context.Context) ([]syncservice.WatchItem, error) {
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return nil, err
	}
	return watchItems(ctx, os.Stderr, st, dl), nil
}

// Reconcile converges this host against origin — pull-merge every peer, then clone or
// idle-sync each repo — and reports how many repos converged and how many were left
// untouched because they were busy. origin is the anti-echo provenance tag the
// converge pass uses to skip the notifying peer.
func (repoConsumer) Reconcile(ctx context.Context, origin string) (syncservice.ReconcileResult, error) {
	converged, skippedBusy, err := reconcileConverged(ctx, origin)
	if err != nil {
		return syncservice.ReconcileResult{}, err
	}
	return syncservice.ReconcileResult{Converged: converged, SkippedBusy: skippedBusy}, nil
}

// Sync runs the same converging reconcile as [repoConsumer.Reconcile]; reposync has no
// distinct sync pass, so both drive reconcile.Reconcile and report the same converged
// and skipped-busy counts.
func (repoConsumer) Sync(ctx context.Context, origin string) (syncservice.SyncResult, error) {
	converged, skippedBusy, err := reconcileConverged(ctx, origin)
	if err != nil {
		return syncservice.SyncResult{}, err
	}
	return syncservice.SyncResult{Converged: converged, SkippedBusy: skippedBusy}, nil
}

// GetState returns the propagating repo registry as opaque JSON, the form a peer
// pull-merges. Read-only and Repos-only: the local-only registry never crosses hosts.
func (repoConsumer) GetState(_ context.Context) (syncservice.RawRegistry, error) {
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	return st.EncodeRepoRegistry()
}

// reconcileConverged runs a converging reconcile pass against origin and tallies the
// repos that landed on disk without error apart from those skipped as busy — a busy
// repo is not an error, but it did not converge either — the counts both Reconcile
// and Sync report.
func reconcileConverged(ctx context.Context, origin string) (converged, skippedBusy int, err error) {
	st, err := state.Load()
	if err != nil {
		return 0, 0, err
	}
	results, err := reconcile.Reconcile(ctx, st, origin)
	if err != nil {
		return 0, 0, err
	}
	for _, r := range results {
		switch {
		case r.Action == reconcile.ActionBusy, r.Action == reconcile.ActionEnvBusy:
			skippedBusy++
		case r.Err == nil:
			converged++
		}
	}
	return converged, skippedBusy, nil
}
