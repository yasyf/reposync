package cli

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

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

// hostsPayload is the shape of `reposync host ls --json`: the schema version,
// this host's identity, and every registered peer, so one call yields identity
// plus peers. Hosts is never nil so it marshals to [] rather than null.
type hostsPayload struct {
	Version int      `json:"version"`
	Self    string   `json:"self"`
	Hosts   []string `json:"hosts"`
}

// writeJSON emits v as a single compact JSON line, the sole thing a --json
// command writes to stdout; logs and warnings go to stderr.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

func newStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect the on-disk reposync state.",
	}
	cmd.AddCommand(newStateGetJSONCmd())
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

func newSelfCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "self",
		Short: "Print this host's ssh identity (user@node) as peers reach it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, err := state.Config.Load()
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
