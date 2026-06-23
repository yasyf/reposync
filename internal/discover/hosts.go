package discover

import (
	"context"

	"github.com/yasyf/synckit/hostregistry"
)

// Hosts enumerates candidate hosts on the network from tailscale and Bonjour,
// dedupes them, and marks which of registered (the peers in the host registry)
// discovery surfaced. Discovery is best-effort: a missing or failing source
// degrades to a SkipNote rather than an error, so Hosts never returns a non-nil
// error.
func Hosts(ctx context.Context, r hostregistry.Runner, registered []string) (HostResult, error) {
	return hostregistry.Hosts(ctx, r, registered)
}
