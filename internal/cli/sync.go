package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/sync"
)

func newSyncCmd() *cobra.Command {
	var repoFilter, origin string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Idle-safe fetch and fast-forward of every registered repo (never pushes).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			results, err := sync.Sync(cmd.Context(), st, repoFilter, origin)
			if err != nil {
				return err
			}
			failed := 0
			for _, r := range results {
				if r.Err != nil {
					fmt.Printf("✗ %s: %v\n", r.Relpath, r.Err)
					failed++
					continue
				}
				fmt.Printf("✓ %s: %s\n", r.Relpath, outcomeLabel(r))
			}
			if failed > 0 {
				return fmt.Errorf("%d repo(s) failed to sync", failed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFilter, "repo", "", "sync only this repo (path or relpath)")
	cmd.Flags().StringVar(&origin, "origin", "", "anti-echo provenance tag from the notifying peer")
	return cmd
}

func outcomeLabel(r sync.Result) string {
	if r.Reason != "" {
		return fmt.Sprintf("%s (%s)", r.Outcome, r.Reason)
	}
	return string(r.Outcome)
}
