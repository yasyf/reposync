package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/config"
	"github.com/yasyf/reposync/internal/service"
)

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install reposync as a launchd agent that syncs on the configured interval",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			plist, err := service.Install(configPath, cfg.Interval.AsDuration())
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "installed launchd agent at %s (every %s)\n", plist, cfg.Interval.AsDuration())
			return nil
		},
	}
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the reposync launchd agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plist, err := service.Uninstall()
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "removed launchd agent at %s\n", plist)
			return nil
		},
	}
}
