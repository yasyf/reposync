package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/config"
	"github.com/yasyf/reposync/internal/sync"
)

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Sync every configured repo with its remote once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			results := sync.All(cmd.Context(), cfg)
			failed := 0
			for _, r := range results {
				if r.Err != nil {
					failed++
					fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s: %v\n", r.Repo.Path, r.Err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s: %s\n", r.Repo.Path, r.State)
			}

			if failed > 0 {
				return fmt.Errorf("%d of %d repos failed to sync", failed, len(results))
			}
			return nil
		},
	}
}
