// Package cli wires the reposync cobra command tree.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/tui"
)

// Execute builds and runs the reposync root command under a context canceled on
// SIGINT/SIGTERM, exiting non-zero on error.
func Execute(version string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := newRoot(version)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "reposync: %v\n", err)
	os.Exit(1)
}

func newRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "reposync",
		Short:         "Keep git repos in sync across your remote hosts.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isInteractive() {
				return cmd.Help()
			}
			return tui.Run(cmd.Context(), version)
		},
	}
	root.AddCommand(
		newRepoCmd(),
		newHostCmd(),
		newSelfCmd(),
		newSyncCmd(),
		newRPCServeCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newTUICmd(version),
	)
	return root
}
