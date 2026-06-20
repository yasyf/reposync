package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/rpc"
	"github.com/yasyf/reposync/internal/state"
)

func newRPCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rpc",
		Short: "Trigger the resident daemon over its unix socket.",
	}
	cmd.AddCommand(newRPCSyncCmd(), newRPCReconcileCmd())
	return cmd
}

func newRPCSyncCmd() *cobra.Command {
	var relpath, origin string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Ask the daemon to idle-sync the registered repos.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sock, err := state.SockPath()
			if err != nil {
				return err
			}
			resp, err := rpc.Sync(cmd.Context(), sock, relpath, origin)
			if err != nil {
				return err
			}
			return printRPC(resp)
		},
	}
	cmd.Flags().StringVar(&relpath, "relpath", "", "sync only this repo (relpath)")
	cmd.Flags().StringVar(&origin, "origin", "", "anti-echo provenance tag from the notifying peer")
	return cmd
}

func newRPCReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Ask the daemon to clone-and-sync every registered repo.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sock, err := state.SockPath()
			if err != nil {
				return err
			}
			resp, err := rpc.Reconcile(cmd.Context(), sock)
			if err != nil {
				return err
			}
			return printRPC(resp)
		},
	}
	return cmd
}

func printRPC(resp *rpc.Response) error {
	if resp.Err != "" {
		return fmt.Errorf("daemon error: %s", resp.Err)
	}
	failed := 0
	for _, r := range resp.Results {
		if r.Err != "" {
			fmt.Printf("✗ %s: %s\n", r.Relpath, r.Err)
			failed++
			continue
		}
		fmt.Printf("✓ %s: %s\n", r.Relpath, rpcOutcomeLabel(r))
	}
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed", failed)
	}
	return nil
}

func rpcOutcomeLabel(r rpc.Result) string {
	if r.Reason != "" {
		return fmt.Sprintf("%s (%s)", r.Outcome, r.Reason)
	}
	return r.Outcome
}
