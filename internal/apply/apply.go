// Package apply performs batched enable/disable of tracked repos: it mutates
// state once, then converges with a single reconcile and a single peer push.
package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

// RepoSelection is a batched enable/disable request: Enable carries discovered
// candidates to start tracking, Disable carries relpaths to stop tracking.
type RepoSelection struct {
	Enable  []discover.Candidate
	Disable []string
}

// Repos applies the selection in one locked state mutation, then converges
// with a single reconcile and a single peer push. It returns the reconcile
// results; peer-propagation failures are joined into the error but never discard
// those results. Disabling removes a repo from tracking only — its on-disk
// checkout is left in place.
func Repos(ctx context.Context, r host.Runner, sel RepoSelection) ([]reconcile.Result, error) {
	st, err := state.Update(func(s *state.State) error {
		for _, c := range sel.Enable {
			s.UpsertRepo(state.Repo{Relpath: c.Relpath, Origin: c.Origin, Trunk: "main", LocalOnly: c.LocalOnly})
		}
		for _, rp := range sel.Disable {
			s.RemoveRepo(rp)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("apply repo selection: %w", err)
	}

	results, err := reconcile.Reconcile(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("reconcile after apply: %w", err)
	}

	var propagateErrs []error
	propagated := false
	for _, c := range sel.Enable {
		if c.LocalOnly || c.Origin == "" {
			continue
		}
		repo := state.Repo{Relpath: c.Relpath, Origin: c.Origin, Trunk: "main"}
		if err := host.PropagateRepo(ctx, st, r, repo); err != nil {
			propagateErrs = append(propagateErrs, fmt.Errorf("propagate %s: %w", c.Relpath, err))
		}
		propagated = true
	}
	if propagated {
		if err := host.RemoteReconcile(ctx, st, r); err != nil {
			propagateErrs = append(propagateErrs, fmt.Errorf("reconcile peers: %w", err))
		}
	}
	return results, errors.Join(propagateErrs...)
}
