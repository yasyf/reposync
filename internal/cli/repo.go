package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Register, list, and unregister tracked repositories.",
	}
	cmd.AddCommand(newRepoAddCmd(), newRepoAddRemoteCmd(), newRepoRmCmd(), newRepoLsCmd())
	return cmd
}

func newRepoAddCmd() *cobra.Command {
	var localOnly bool
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a repo, propagate it to peers, and converge it everywhere.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoAdd(cmd.Context(), args[0], localOnly)
		},
	}
	cmd.Flags().BoolVar(&localOnly, "local-only", false, "track this repo on this host only (no origin required, never propagated)")
	return cmd
}

func runRepoAdd(ctx context.Context, path string, localOnly bool) error {
	abspath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path %s: %w", path, err)
	}

	st, err := state.Load()
	if err != nil {
		return err
	}
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return err
	}
	relpath, err := relUnder(dl, abspath)
	if err != nil {
		return err
	}

	repoVCS, err := vcs.Open(abspath, "main")
	if err != nil {
		return err
	}
	origin, err := repoVCS.Origin(ctx)
	switch {
	case errors.Is(err, vcs.ErrNoOrigin):
		if !localOnly {
			return fmt.Errorf("repo has no origin remote; cannot converge across hosts — use --local-only to track it on this host only")
		}
		origin = ""
	case err != nil:
		return err
	}

	repo := state.Repo{Relpath: relpath, Origin: origin, Trunk: "main", LocalOnly: localOnly}
	st, err = state.Update(func(s *state.State) error {
		s.UpsertRepo(repo)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("registered %s (origin %s)\n", relpath, originLabel(repo))

	if localOnly {
		return nil
	}

	runner := host.NewExecRunner()
	if err := host.PropagateRepo(ctx, st, runner, repo); err != nil {
		fmt.Printf("WARN propagate %s to peers: %v\n", relpath, err)
	}
	results, err := reconcile.Reconcile(ctx, st)
	if err != nil {
		return err
	}
	if err := printReconcile(results); err != nil {
		return err
	}
	if err := host.RemoteReconcile(ctx, st, runner); err != nil {
		fmt.Printf("WARN reconcile peers: %v\n", err)
	}
	return nil
}

func newRepoAddRemoteCmd() *cobra.Command {
	var origin, relpath, trunk string
	cmd := &cobra.Command{
		Use:   "add-remote",
		Short: "Idempotently upsert a repo by origin (used for peer propagation).",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := state.Update(func(s *state.State) error {
				s.UpsertRepo(state.Repo{Relpath: relpath, Origin: origin, Trunk: trunk})
				return nil
			})
			return err
		},
	}
	cmd.Flags().StringVar(&origin, "origin", "", "origin remote URL")
	cmd.Flags().StringVar(&relpath, "relpath", "", "path relative to default_location")
	cmd.Flags().StringVar(&trunk, "trunk", "main", "trunk branch")
	cmd.MarkFlagRequired("origin")
	cmd.MarkFlagRequired("relpath")
	return cmd
}

func newRepoRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <path>",
		Short: "Unregister a repo (does not delete the checkout).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			relpath, err := repoRelpath(st, args[0])
			if err != nil {
				return err
			}
			if _, err := state.Update(func(s *state.State) error {
				s.RemoveRepo(relpath)
				return nil
			}); err != nil {
				return err
			}
			fmt.Printf("unregistered %s\n", relpath)
			return nil
		},
	}
	return cmd
}

func newRepoLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List registered repos.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "RELPATH\tORIGIN\tTRUNK\tLOCAL_ONLY")
			for _, r := range st.Repos {
				fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", r.Relpath, originLabel(r), r.Trunk, r.LocalOnly)
			}
			return w.Flush()
		},
	}
	return cmd
}

// repoRelpath resolves arg to a registered relpath, accepting either a path
// under default_location or a bare relpath.
func repoRelpath(st *state.State, arg string) (string, error) {
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return "", err
	}
	abspath, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", arg, err)
	}
	if rel, err := relUnder(dl, abspath); err == nil {
		return rel, nil
	}
	return arg, nil
}

// relUnder returns abspath relative to dl, erroring when abspath escapes dl.
func relUnder(dl, abspath string) (string, error) {
	rel, err := filepath.Rel(dl, abspath)
	if err != nil {
		return "", fmt.Errorf("relativize %s under %s: %w", abspath, dl, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repo must live under default_location %s", dl)
	}
	return rel, nil
}

func originLabel(r state.Repo) string {
	if r.Origin == "" {
		return "(local-only)"
	}
	return r.Origin
}
