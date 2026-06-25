package cli

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
)

func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "List the shared host mesh (managed by synckitd).",
	}
	cmd.AddCommand(newHostLsCmd())
	return cmd
}

// newHostLsCmd is a thin shim over `synckitd host ls`: the shared host mesh is owned
// by synckitd, so reposync forwards the listing rather than reading the mesh itself.
func newHostLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the registered peer hosts by forwarding to synckitd.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"host", "ls"}
			if asJSON {
				args = append(args, "--json")
			}
			//nolint:gosec // G204: fixed argv forwarding to the synckitd binary on PATH, no user-supplied command.
			sub := exec.CommandContext(cmd.Context(), "synckitd", args...)
			sub.Stdout = cmd.OutOrStdout()
			sub.Stderr = cmd.ErrOrStderr()
			if err := sub.Run(); err != nil {
				return fmt.Errorf("synckitd host ls: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "forward synckitd's machine-readable JSON listing")
	return cmd
}
