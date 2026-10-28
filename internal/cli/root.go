// Package cli wires the reposync cobra command tree.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	dkversion "github.com/yasyf/daemonkit/version"

	"github.com/yasyf/reposync/internal/tui"
)

// Execute builds and runs the reposync root command under a context canceled on
// SIGINT/SIGTERM, exiting non-zero on error.
func Execute(version string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := newRoot(dkversion.Running(version))
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
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if !ensuresHelper(cmd) {
				return nil
			}
			if _, err := ensureHelper(cmd.Context(), version); err != nil {
				return fmt.Errorf("ensure the resident helper: %w", err)
			}
			return nil
		},
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
		newRPCServeCmd(version),
		newInstallCmd(version),
		newUninstallCmd(),
		newTUICmd(version),
	)
	return root
}

func ensuresHelper(cmd *cobra.Command) bool {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	switch cmd.Name() {
	case rpcServeCommand, "install", "uninstall", "help", "completion",
		cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return false
	}
	return true
}
