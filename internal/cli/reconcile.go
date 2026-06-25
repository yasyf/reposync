package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

func newReconcileCmd() *cobra.Command {
	var origin string
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Clone every missing repo and idle-sync every present one.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			results, err := reconcile.Reconcile(cmd.Context(), st, origin)
			if err != nil {
				return err
			}
			if err := printReconcile(results); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&origin, "origin", "", "anti-echo provenance tag from the notifying peer")
	return cmd
}

func printReconcile(results []reconcile.Result) error {
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("✗ %s %s: %v\n", r.Action, r.Relpath, r.Err)
			failed++
			continue
		}
		fmt.Printf("%s %s\n", r.Action, r.Relpath)
	}
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed to reconcile", failed)
	}
	return nil
}
