package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yasyf/synckit/hostregistry"

	"github.com/yasyf/reposync/internal/state"
)

// jsonVersion is the literal schema version stamped on every --json payload.
// Bump only on a breaking change; fields are otherwise additive-only so a
// cross-language consumer pinned to version 1 keeps parsing.
const jsonVersion = 1

// selfPayload is the shape of `reposync self --json`: the schema version and
// this host's ssh identity (user@node), empty until a peer is registered.
type selfPayload struct {
	Version int    `json:"version"`
	Self    string `json:"self"`
}

// writeJSON emits v as a single compact JSON line, the sole thing a --json
// command writes to stdout; logs and warnings go to stderr.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

// appliedPayload is the shape of `reposync state apply-json`: the count of present
// entries persisted from the merged registry on stdin.
type appliedPayload struct {
	Applied int `json:"applied"`
}

func newStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect the on-disk reposync state.",
	}
	cmd.AddCommand(newStateGetJSONCmd(), newStateApplyJSONCmd())
	return cmd
}

func newStateGetJSONCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-json",
		Short: "Print the convergent repo registry (origin-keyed, with tombstones) as JSON.",
		Long: "Read state.json directly — no daemon socket — and print this host's " +
			"propagating repo registry as JSON, the form a peer pull-merges. Local-only " +
			"repos are excluded. Daemon-independent, so a peer can read it while this " +
			"host's daemon is down.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			data, err := st.EncodeRepoRegistry()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(append(data, '\n'))
			return err
		},
	}
	return cmd
}

func newStateApplyJSONCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply-json",
		Short: "Read a merged propagating repo registry from stdin and persist it as JSON.",
		Long: "Read a merged convergent repo registry (origin-keyed, with tombstones) from " +
			"stdin and persist it as this host's propagating registry, leaving the local-only " +
			"repos, settings, and default location untouched. This is the write half of the " +
			"pull-merge synckitd drives after fetching every peer's get-json.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read merged registry from stdin: %w", err)
			}
			merged, err := state.DecodeRepoRegistry(raw)
			if err != nil {
				return err
			}
			if _, err := state.Update(cmd.Context(), func(s *state.State) error {
				s.Repos = merged
				return nil
			}); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), appliedPayload{Applied: len(merged.Present())})
		},
	}
	return cmd
}

func newSelfCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "self",
		Short: "Print this host's ssh identity (user@node) as peers reach it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, err := hostregistry.Mesh.Load()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), selfPayload{Version: jsonVersion, Self: reg.Self})
			}
			_, err = cmd.OutOrStdout().Write([]byte(reg.Self + "\n"))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable JSON line on stdout")
	return cmd
}
