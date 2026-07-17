// Package registry exposes read-only access to the reposync repo registry.
package registry

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/yasyf/reposync/internal/state"
)

// Repo describes a repository in the reposync registry.
type Repo struct {
	// Relpath is the path relative to DefaultLocation.
	Relpath string
	// Path is DefaultLocation joined with Relpath. It is expanded but not symlink-canonicalized.
	Path string
	// Origin is the repository origin, or empty for a local-only repository.
	Origin string
	// Trunk is the repository's trunk branch.
	Trunk string
	// LocalOnly reports whether the repository is local to this host.
	LocalOnly bool
	// NoEnvSync reports whether environment-file synchronization is disabled.
	NoEnvSync bool
}

// Registry is a read-only snapshot of the reposync repo registry.
type Registry struct {
	// DefaultLocation is the absolute default repository location with the home directory expanded.
	DefaultLocation string
	// Repos contains the non-tombstoned repositories sorted by Relpath.
	Repos []Repo
}

// Load reads the reposync repo registry. It returns an empty Registry when the
// state file does not exist.
func Load() (Registry, error) {
	path, err := state.Path()
	if err != nil {
		return Registry{}, fmt.Errorf("resolve state path: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Registry{}, nil
	} else if err != nil {
		return Registry{}, fmt.Errorf("stat state %s: %w", path, err)
	}

	st, err := state.Load()
	if err != nil {
		return Registry{}, fmt.Errorf("load state: %w", err)
	}
	defaultLocation, err := st.DefaultLocationExpanded()
	if err != nil {
		return Registry{}, fmt.Errorf("expand default location: %w", err)
	}

	stateRepos := st.AllRepos()
	result := Registry{
		DefaultLocation: defaultLocation,
		Repos:           make([]Repo, 0, len(stateRepos)),
	}
	for _, r := range stateRepos {
		result.Repos = append(result.Repos, Repo{
			Relpath:   r.Relpath,
			Path:      r.AbsPath(defaultLocation),
			Origin:    r.Origin,
			Trunk:     r.Trunk,
			LocalOnly: r.LocalOnly,
			NoEnvSync: r.NoEnvSync,
		})
	}
	sort.Slice(result.Repos, func(i, j int) bool {
		return result.Repos[i].Relpath < result.Repos[j].Relpath
	})
	return result, nil
}
