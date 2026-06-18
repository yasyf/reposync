package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const sampleConfig = `# reposync configuration
# How often the launchd agent triggers a sync.
interval: 5m

# Repositories to keep in sync with one of their git remotes.
repos:
  - path: ~/Code/example
    remote: origin   # optional, defaults to "origin"
    branch: main     # optional, defaults to the current branch
    auto_commit: false
`

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and scaffold the reposync config",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newConfigInitCmd(), newConfigPathCmd())
	return cmd
}

func newConfigInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write a starter config to the config path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(configPath); err == nil {
				return fmt.Errorf("%s already exists", configPath)
			}
			if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(configPath, []byte(sampleConfig), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote starter config to %s\n", configPath)
			return nil
		},
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config path reposync reads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), configPath)
			return nil
		},
	}
}
