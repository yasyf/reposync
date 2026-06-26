package cli

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/yasyf/synckit/hostregistry"
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
