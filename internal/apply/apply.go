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
	"github.com/yasyf/reposync/internal/vcs"
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
	enabled, err := enabledRepos(ctx, sel.Enable)
	if err != nil {
		return nil, fmt.Errorf("apply repo selection: %w", err)
	}

	st, err := state.Update(ctx, func(s *state.State) error {
		for _, r := range enabled {
			s.AddRepo(r)
		}
		for _, rp := range sel.Disable {
			s.RemoveRepo(rp)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("apply repo selection: %w", err)
	}

	results, err := reconcile.Repos(ctx, st, enabled)
	if err != nil {
		return nil, fmt.Errorf("reconcile after apply: %w", err)
	}
	return results, nil
}

// enabledRepos turns candidates into registry entries, resolving each one's trunk
// from its checkout. Detection runs here rather than during discovery: it can cost
// a remote round-trip, which belongs to an explicit enable, not a read-only scan.
func enabledRepos(ctx context.Context, candidates []discover.Candidate) ([]state.Repo, error) {
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return nil, err
	}
	enabled := make([]state.Repo, 0, len(candidates))
	for _, c := range candidates {
		r := trackedRepo(c)
		r.Trunk = vcs.DetectTrunk(ctx, r.AbsPath(dl))
		enabled = append(enabled, r)
	}
	return enabled, nil
}

// trackedRepo builds a candidate's registry entry. Each branch writes one of the
// two identities the registries hold — a local-only repo is keyed by its relpath
// and carries no origin, a propagating repo is keyed by its origin — so neither
// can spell the entry the state layer rejects.
func trackedRepo(c discover.Candidate) state.Repo {
	if c.LocalOnly {
		return state.Repo{Relpath: c.Relpath, LocalOnly: true, NoEnvSync: c.NoEnvSync}
	}
	return state.Repo{Relpath: c.Relpath, Origin: c.Origin, NoEnvSync: c.NoEnvSync}
}
