// Package cli wires up the reposync command-line interface.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/config"
)

// configPath holds the value of the persistent --config flag.
var configPath string

func newRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "reposync",
		Short:         "Keep git repos in sync across your remote hosts",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultPath(), "path to the config file")
	root.AddCommand(newSyncCmd(), newInstallCmd(), newUninstallCmd(), newConfigCmd())
	return root
}

// Execute runs the root command and exits non-zero on error.
func Execute(version string) {
	if err := newRootCmd(version).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "reposync:", err)
		os.Exit(1)
	}
}
