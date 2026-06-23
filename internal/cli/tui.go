package cli

import (
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yasyf/reposync/hostregistry"
	"github.com/yasyf/reposync/internal/tui"
)

// isInteractive reports whether stdin is a terminal, gating the bare-command
// TUI launch.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func newTUICmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:    "tui",
		Short:  "Launch the interactive TUI.",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tui.Run(cmd.Context(), tui.Options{Version: version, Runner: hostregistry.NewExecRunner()})
		},
	}
}
