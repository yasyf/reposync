package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/service"
	"github.com/yasyf/reposync/internal/state"
)

func newInstallCmd() *cobra.Command {
	var tickOnly bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the launchd reconcile tick and watch daemon LaunchAgents.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := service.Install(cmd.Context(), service.NewLauncher(), tickOnly); err != nil {
				return err
			}
			return printInstalled(tickOnly)
		},
	}
	cmd.Flags().BoolVar(&tickOnly, "tick-only", false, "install only the reconcile tick, not the watch daemon")
	return cmd
}

func newUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Unload and remove the reposync LaunchAgents.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := service.Uninstall(cmd.Context(), service.NewLauncher()); err != nil {
				return err
			}
			fmt.Println("uninstalled reposync LaunchAgents")
			return nil
		},
	}
	return cmd
}

func printInstalled(tickOnly bool) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	agents := filepath.Join(home, "Library", "LaunchAgents")
	fmt.Printf("installed tick %s (every %s)\n", filepath.Join(agents, service.TickLabel+".plist"), time.Duration(st.Settings.Interval))
	if !tickOnly {
		fmt.Printf("installed watch %s\n", filepath.Join(agents, service.WatchLabel+".plist"))
	}
	return nil
}
