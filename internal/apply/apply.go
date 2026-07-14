// Package apply performs batched enable/disable of tracked repos: it mutates the
// convergent registry once, then brings the newly enabled repos onto disk with a
// single reconcile. Removals are tombstones in the registry, so they — like adds —
// converge to every peer on its next pull-merge; apply never pushes to a peer.
package apply

import (
	"context"
	"fmt"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

// RepoSelection is a batched enable/disable request: Enable carries discovered
// candidates to start tracking, Disable carries relpaths to stop tracking.
type RepoSelection struct {
	Enable  []discover.Candidate
	Disable []string
}

// Repos applies the selection in one locked registry mutation — adds for Enable,
// tombstones for Disable — then clones the newly enabled repos onto disk with a
// single reconcile of just that subset. Adds and removals converge to peers via
// pull-merge on their own schedule, so there is no peer push here. Disabling
// tombstones a repo's registry entry only; its on-disk checkout is left in place.
func Repos(ctx context.Context, sel RepoSelection) ([]reconcile.Result, error) {
	st, err := state.Update(ctx, func(s *state.State) error {
		for _, c := range sel.Enable {
			s.AddRepo(state.Repo{Relpath: c.Relpath, Origin: c.Origin, Trunk: "main", LocalOnly: c.LocalOnly, NoEnvSync: c.NoEnvSync})
		}
		for _, rp := range sel.Disable {
			s.RemoveRepo(rp)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("apply repo selection: %w", err)
	}

	enabled := make([]state.Repo, 0, len(sel.Enable))
	for _, c := range sel.Enable {
		enabled = append(enabled, state.Repo{Relpath: c.Relpath, Origin: c.Origin, Trunk: "main", LocalOnly: c.LocalOnly, NoEnvSync: c.NoEnvSync})
	}
	results, err := reconcile.Repos(ctx, st, enabled)
	if err != nil {
		return nil, fmt.Errorf("reconcile after apply: %w", err)
	}
	return results, nil
}
