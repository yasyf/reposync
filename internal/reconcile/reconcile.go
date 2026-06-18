// Package reconcile ensures every registered repo is present on this host and
// then idle-syncs it. Missing repos are cloned as colocated jj into a temp dir
// and atomically renamed into place; a pre-existing non-repo directory at the
// destination surfaces as a collision error rather than being overwritten. The
// whole pass is serialized per host by a flock. It never clones over an
// existing repo and is safe to run repeatedly.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/sync"
	"github.com/yasyf/reposync/internal/vcs"
)

const tmpDirName = ".reposync-tmp"

const (
	// ActionCloned means the repo was absent and was cloned into place.
	ActionCloned = "cloned"
	// ActionPresent means the repo was already present and was idle-synced.
	ActionPresent = "present"
	// ActionSkippedLocalOnly means a local-only repo was absent and cannot be cloned.
	ActionSkippedLocalOnly = "skipped-local-only"
	// ActionSkippedNoOrigin means an absent repo has no origin and cannot be cloned.
	ActionSkippedNoOrigin = "skipped-no-origin"
)

// Result reports what Reconcile did for one registered repo.
type Result struct {
	Relpath string
	Action  string
	Err     error
}

// Reconcile clones every absent registered repo and idle-syncs every present
// one, holding the per-host reconcile flock for the whole pass.
func Reconcile(ctx context.Context, st *state.State) ([]Result, error) {
	var results []Result
	err := state.WithLock(func() error {
		dl, err := st.DefaultLocationExpanded()
		if err != nil {
			return err
		}
		tmpRoot := filepath.Join(dl, tmpDirName)
		defer os.RemoveAll(tmpRoot)

		results = make([]Result, len(st.Repos))
		for i, repo := range st.Repos {
			results[i] = reconcileOne(ctx, st, repo, dl, tmpRoot)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func reconcileOne(ctx context.Context, st *state.State, repo state.Repo, dl, tmpRoot string) Result {
	abspath := repo.AbsPath(dl)
	if present(abspath) {
		return Result{Relpath: repo.Relpath, Action: ActionPresent, Err: idleSync(ctx, st, abspath)}
	}
	if repo.LocalOnly {
		return Result{Relpath: repo.Relpath, Action: ActionSkippedLocalOnly}
	}
	if repo.Origin == "" {
		return Result{Relpath: repo.Relpath, Action: ActionSkippedNoOrigin}
	}
	if err := clone(ctx, repo, abspath, tmpRoot); err != nil {
		return Result{Relpath: repo.Relpath, Action: ActionCloned, Err: err}
	}
	return Result{Relpath: repo.Relpath, Action: ActionCloned, Err: idleSync(ctx, st, abspath)}
}

// clone clones repo.Origin into a unique temp dir, verifies it is a colocated jj
// clone of the expected origin, then atomically renames it onto abspath. A
// pre-existing non-repo directory at abspath makes the rename fail loudly.
func clone(ctx context.Context, repo state.Repo, abspath, tmpRoot string) error {
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return fmt.Errorf("create temp root %s: %w", tmpRoot, err)
	}
	parent, err := os.MkdirTemp(tmpRoot, "clone-*")
	if err != nil {
		return fmt.Errorf("create temp clone dir: %w", err)
	}
	defer os.RemoveAll(parent)
	tmp := filepath.Join(parent, filepath.Base(repo.Relpath))

	if err := vcs.Clone(ctx, repo.Origin, tmp); err != nil {
		return err
	}
	if err := verifyClone(ctx, tmp, repo.Origin); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abspath), 0o755); err != nil {
		return fmt.Errorf("create parent of %s: %w", abspath, err)
	}
	if err := os.Rename(tmp, abspath); err != nil {
		return fmt.Errorf("place clone at %s (destination may already exist): %w", abspath, err)
	}
	return nil
}

// verifyClone confirms tmp is a colocated jj checkout whose origin matches want.
func verifyClone(ctx context.Context, tmp, want string) error {
	if _, err := os.Stat(filepath.Join(tmp, ".jj")); err != nil {
		return fmt.Errorf("clone %s is not colocated jj: %w", tmp, err)
	}
	r, err := vcs.Open(tmp, "")
	if err != nil {
		return fmt.Errorf("open clone %s: %w", tmp, err)
	}
	got, err := r.Origin(ctx)
	if err != nil {
		return fmt.Errorf("read clone origin at %s: %w", tmp, err)
	}
	if got != want {
		return fmt.Errorf("clone origin mismatch at %s: got %q, want %q", tmp, got, want)
	}
	return nil
}

// idleSync runs the idle-safe sync for the single repo at abspath, reusing
// internal/sync so reconcile never duplicates the vcs flow.
func idleSync(ctx context.Context, st *state.State, abspath string) error {
	results, err := sync.Sync(ctx, st, abspath, "")
	if err != nil {
		return err
	}
	var errs []error
	for _, res := range results {
		if res.Err != nil {
			errs = append(errs, res.Err)
		}
	}
	return errors.Join(errs...)
}

// present reports whether a repo checkout (jj or git) already exists at abspath.
func present(abspath string) bool {
	for _, marker := range []string{".jj", ".git"} {
		if _, err := os.Stat(filepath.Join(abspath, marker)); err == nil {
			return true
		}
	}
	return false
}
