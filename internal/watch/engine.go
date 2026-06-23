package watch

import (
	"time"

	"github.com/yasyf/reposync/internal/state"
	synckitwatch "github.com/yasyf/synckit/watch"
)

// newEngine wires reposync's domain layer into the generic synckit watch engine,
// fixing the identity type to state.Repo. The git-backed resolver and the
// rpc-over-ssh notifier satisfy synckit's Resolver/Notifier structurally; a repo's
// relpath is its stable digest key, since it is host-agnostic and never changes for
// a tracked repo. The engine keeps the anti-echo core (debounce, dedupe by resolved
// origin-trunk hash, record-before-notify, concurrent peer fan-out); reposync only
// supplies what a repo means.
func newEngine(resolver synckitwatch.Resolver[state.Repo], notifier synckitwatch.Notifier[state.Repo], debounce time.Duration, hosts []string) *synckitwatch.Engine[state.Repo] {
	return synckitwatch.NewEngine[state.Repo](resolver, notifier, func(repo state.Repo) string { return repo.Relpath }, debounce, hosts)
}
