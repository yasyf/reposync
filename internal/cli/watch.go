package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/watch"
)

func newWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run the event-based watch daemon, notifying peers on trunk changes.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			return watch.Watch(cmd.Context(), st)
		},
	}
	return cmd
}
