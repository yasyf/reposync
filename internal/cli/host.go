package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/service"
	"github.com/yasyf/reposync/internal/state"
)

func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Register, list, and unregister peer hosts.",
	}
	cmd.AddCommand(newHostAddCmd(), newHostRmCmd(), newHostLsCmd(), newHostVerifyCmd())
	return cmd
}

func newHostAddCmd() *cobra.Command {
	var self string
	var noRecurse bool
	cmd := &cobra.Command{
		Use:   "add <user@node>",
		Short: "Bootstrap reposync on a peer: install, register, share state, converge.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			log, err := host.AddHost(cmd.Context(), st, host.NewExecRunner(), args[0], self, noRecurse)
			for _, line := range log {
				fmt.Println(line)
			}
			if err != nil {
				return err
			}
			// Bring this host online too: adding a peer should leave the local
			// reconcile tick and watch daemon running without a separate install.
			// Skipped on the no-recurse inverse add, which the bootstrap installs
			// over ssh.
			if !noRecurse {
				if err := service.Install(cmd.Context(), service.NewLauncher(), false); err != nil {
					fmt.Printf("WARN install local services: %v\n", err)
				} else {
					fmt.Println("installed local services")
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&self, "self", "", "how peers reach this host (default: auto-detect via tailscale)")
	cmd.Flags().BoolVar(&noRecurse, "no-recurse", false, "register only, skip remote bootstrap (loop guard for inverse registration)")
	return cmd
}

func newHostRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <user@node>",
		Short: "Unregister a peer host.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := host.RemoveHost(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("unregistered host %s\n", args[0])
			return nil
		},
	}
	return cmd
}

func newHostLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List registered peer hosts.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "HOST")
			for _, h := range st.Hosts {
				_, _ = fmt.Fprintln(w, h)
			}
			return w.Flush()
		},
	}
	return cmd
}

func newHostVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Probe each registered host for ssh reachability, reposync install, and version.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			results := host.VerifyAll(cmd.Context(), host.NewExecRunner(), st.Hosts)
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "HOST\tREACHABLE\tBOOTSTRAPPED\tVERSION")
			for _, v := range results {
				version := v.Version
				if version == "" {
					version = "-"
				}
				_, _ = fmt.Fprintf(w, "%s\t%t\t%t\t%s\n", v.Target, v.Reachable, v.Bootstrapped, version)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, v := range results {
				if v.Err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %v\n", v.Target, v.Err)
				}
			}
			return nil
		},
	}
	return cmd
}
