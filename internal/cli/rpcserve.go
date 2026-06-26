package cli

import (
	"context"
	"io"
	"log"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/synckit/rpc"
	"github.com/yasyf/synckit/syncservice"

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

			d := rpc.NewDispatcher()
			syncservice.RegisterConsumer(d, repoConsumer{})
			rw := struct {
				io.Reader
				io.Writer
			}{os.Stdin, os.Stdout}
			return rpc.ServeConn(cmd.Context(), rw, d)
		},
	}
	return cmd
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
// idle-sync each repo — and reports how many repos converged. origin is the anti-echo
// provenance tag the converge pass uses to skip the notifying peer.
func (repoConsumer) Reconcile(ctx context.Context, origin string) (syncservice.ReconcileResult, error) {
	converged, err := reconcileConverged(ctx, origin)
	if err != nil {
		return syncservice.ReconcileResult{}, err
	}
	return syncservice.ReconcileResult{Converged: converged}, nil
}

// Sync runs the same converging reconcile as [repoConsumer.Reconcile]; reposync has no
// distinct sync pass, so both drive reconcile.Reconcile and report the converged
// count.
func (repoConsumer) Sync(ctx context.Context, origin string) (syncservice.SyncResult, error) {
	converged, err := reconcileConverged(ctx, origin)
	if err != nil {
		return syncservice.SyncResult{}, err
	}
	return syncservice.SyncResult{Converged: converged}, nil
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

// reconcileConverged runs a converging reconcile pass against origin and counts the
// repos that landed on disk without error, the converged tally both Reconcile and
// Sync report.
func reconcileConverged(ctx context.Context, origin string) (int, error) {
	st, err := state.Load()
	if err != nil {
		return 0, err
	}
	results, err := reconcile.Reconcile(ctx, st, origin)
	if err != nil {
		return 0, err
	}
	converged := 0
	for _, r := range results {
		if r.Err == nil {
			converged++
		}
	}
	return converged, nil
}
