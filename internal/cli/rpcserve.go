package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/worker"

	"github.com/yasyf/synckit/helperruntime"
	"github.com/yasyf/synckit/rpc"
	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/transfer"
)

const residentProcessLimit = 16

func newRPCServeCmd(build string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rpc-serve-v1",
		Short: "Run the resident revisioned sync service for synckitd.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runResidentService(cmd.Context(), build)
		},
	}
	return cmd
}

type residentProduct struct{}

func (residentProduct) Drain(context.Context) error { return nil }
func (residentProduct) Close(context.Context) error { return nil }

func runResidentService(ctx context.Context, build string) error {
	if err := state.Initialize(ctx); err != nil {
		return err
	}
	directory, err := state.Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	generation, err := proc.ProcessGeneration()
	if err != nil {
		return err
	}
	workers, err := worker.NewPool(worker.Config{
		Capacity: residentProcessLimit, QueueCapacity: residentProcessLimit,
		MaxTotalRun: 12 * time.Minute, MaxStdinBytes: 16 << 20,
		MaxStdoutBytes: 16 << 20, MaxStderrBytes: 1 << 20,
	}, &proc.Reaper{Store: &proc.FileStore{Path: filepath.Join(directory, "resident-workers.db")}, Generation: generation})
	if err != nil {
		return err
	}
	children, err := proc.NewManager(residentProcessLimit, &proc.Reaper{
		Store: &proc.FileStore{Path: filepath.Join(directory, "resident-children.db")}, Generation: generation,
	})
	if err != nil {
		return err
	}
	socket, err := state.SockPath()
	if err != nil {
		return err
	}
	runtime, err := helperruntime.New(helperruntime.Config{
		App: helperruntime.App{Name: state.ToolName, RuntimeBuild: build}, Socket: socket,
		Server: rpc.NewServer(newServeDispatcher()), Workers: workers, Children: children,
		StopStore: &proc.FileStore{Path: filepath.Join(directory, "resident-stop.db")},
		Prepare:   func(dkdaemon.Activation) (helperruntime.Product, error) { return residentProduct{}, nil },
	})
	if err != nil {
		return err
	}
	err = runtime.Run(ctx)
	if ctx.Err() != nil && (err == nil || errors.Is(err, ctx.Err())) {
		return nil
	}
	return err
}

// newServeDispatcher builds the exact resident Synckit service dispatcher.
func newServeDispatcher() *rpc.Dispatcher {
	d := rpc.NewDispatcher()
	syncservice.RegisterConsumer(d, repoConsumer{})
	return d
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
func (repoConsumer) Reconcile(ctx context.Context, _ string) (syncservice.ReconcileResult, error) {
	converged, skippedBusy, err := reconcileConverged(ctx)
	if err != nil {
		return syncservice.ReconcileResult{}, err
	}
	return syncservice.ReconcileResult{Converged: converged, SkippedBusy: skippedBusy}, nil
}

func (repoConsumer) Export(ctx context.Context, request syncservice.ExportRequest) (syncservice.ChangeEnvelope, error) {
	return (transfer.Service{}).Export(ctx, request)
}

func (repoConsumer) Apply(ctx context.Context, change syncservice.ChangeEnvelope) (syncservice.ApplyResult, error) {
	return (transfer.Service{}).Apply(ctx, change)
}

// reconcileConverged runs a converging reconcile pass against origin and tallies the
// repos that landed on disk without error apart from those skipped as busy — a busy
// repo is not an error, but it did not converge either — the counts both Reconcile
// and Sync report.
func reconcileConverged(ctx context.Context) (converged, skippedBusy int, err error) {
	st, err := state.Load()
	if err != nil {
		return 0, 0, err
	}
	results, err := reconcile.Reconcile(ctx, st)
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
