package discover

import (
	"context"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/synckit/hostregistry"
)

// Hosts enumerates candidate hosts on the network from tailscale and Bonjour,
// dedupes them, and marks which are already registered in st.Hosts. Discovery is
// best-effort: a missing or failing source degrades to a SkipNote rather than an
// error, so Hosts never returns a non-nil error.
func Hosts(ctx context.Context, r hostregistry.Runner, st *state.State) (HostResult, error) {
	return hostregistry.Hosts(ctx, r, st.Hosts)
}
