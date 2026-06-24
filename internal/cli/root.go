// Package cli wires the reposync cobra command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/tui"
	"github.com/yasyf/synckit/hostregistry"
)

// statusError carries a process exit code out of a command so the remote status
// of `host exec` propagates to reposync's own exit code. Its message is empty so
// Execute prints nothing extra when honoring the code.
type statusError int

func (e statusError) Error() string { return "" }

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
	var status statusError
	if errors.As(err, &status) {
		os.Exit(int(status))
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
			return tui.Run(cmd.Context(), tui.Options{Version: version, Runner: hostregistry.NewExecRunner()})
		},
	}
	root.AddCommand(
		newRepoCmd(),
		newHostCmd(),
		newSelfCmd(),
		newStateCmd(),
		newSyncCmd(),
		newReconcileCmd(),
		newRPCCmd(),
		newWatchCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newTUICmd(version),
	)
	return root
}
