// Package cli wires the reposync cobra command tree.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// Execute builds and runs the reposync root command under a context canceled on
// SIGINT/SIGTERM, exiting non-zero on error.
func Execute(version string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := newRoot(version)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "reposync: %v\n", err)
		os.Exit(1)
	}
}

func newRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "reposync",
		Short:         "Keep git repos in sync across your remote hosts.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newRepoCmd(),
		newHostCmd(),
		newSyncCmd(),
		newReconcileCmd(),
		newRPCCmd(),
		newWatchCmd(),
		newInstallCmd(),
		newUninstallCmd(),
	)
	return root
}
