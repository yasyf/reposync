// Package sync runs the idle-safe per-repo fetch-and-advance flow over every
// registered repo, composing internal/vcs. It never pushes and never clobbers
// in-progress work: a busy or non-trunk repo is left untouched.
package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

const concurrency = 8

// Result reports what Sync did to one registered repo.
type Result struct {
	Relpath string
	Outcome vcs.Outcome
	Reason  string
	Err     error
}

// Sync advances every registered repo onto its trunk, idle-safe and never
// pushing. When repoFilter is non-empty only the repo whose absolute path or
// relpath matches it is synced; an unmatched filter is an error. origin is the
// optional anti-echo provenance tag from the watcher, currently advisory.
func Sync(ctx context.Context, st *state.State, repoFilter, _ string) ([]Result, error) {
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return nil, err
	}

	targets, err := selectRepos(st, dl, repoFilter)
	if err != nil {
		return nil, err
	}

	idle := time.Duration(st.Settings.IdleThreshold)
	repoOpTimeout := time.Duration(st.Settings.RepoOpTimeout)
	results := make([]Result, len(targets))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, repo := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, repo state.Repo) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = syncOne(ctx, repo, repo.AbsPath(dl), idle, repoOpTimeout)
		}(i, repo)
	}
	wg.Wait()
	return results, nil
}

func selectRepos(st *state.State, dl, repoFilter string) ([]state.Repo, error) {
	if repoFilter == "" {
		return st.Repos, nil
	}
	absFilter, err := filepath.Abs(repoFilter)
	if err != nil {
		return nil, err
	}
	for _, repo := range st.Repos {
		if repo.Relpath == repoFilter || repo.AbsPath(dl) == absFilter {
			return []state.Repo{repo}, nil
		}
	}
	return nil, fmt.Errorf("repo not registered: %s", repoFilter)
}

func syncOne(ctx context.Context, repo state.Repo, abspath string, idle, repoOpTimeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, repoOpTimeout)
	defer cancel()

	res := Result{Relpath: repo.Relpath}

	r, err := vcs.Open(abspath, repo.Trunk)
	if err != nil {
		res.Err = err
		return res
	}

	busy, reason, err := r.InUse(ctx, idle)
	if err != nil {
		res.Err = err
		return res
	}
	if busy {
		res.Outcome = vcs.OutcomeBusy
		res.Reason = reason
		return res
	}

	hasTrunk, err := r.HasTrunk(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	if !hasTrunk {
		res.Outcome = vcs.OutcomeNoTrunk
		return res
	}

	outcome, err := r.Advance(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	res.Outcome = outcome
	return res
}
