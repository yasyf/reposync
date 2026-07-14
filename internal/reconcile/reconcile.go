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
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/yasyf/synckit/hostregistry"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/sync"
	"github.com/yasyf/reposync/internal/vcs"
)

// TmpDirName is the bookkeeping subdirectory reconcile creates under the
// default location for staging clones; scanners should skip it.
const TmpDirName = ".reposync-tmp"

const (
	// ActionCloned means the repo was absent and was cloned into place.
	ActionCloned = "cloned"
	// ActionPresent means the repo was already present and was idle-synced.
	ActionPresent = "present"
	// ActionBusy means the repo was present but in use, so the idle-sync left it untouched.
	ActionBusy = "busy"
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

// Reconcile converges the propagating repo registry with every peer and then brings
// this host into line: it pull-merges each peer's registry, persists the converged
// set, clones every absent propagating repo and idle-syncs every present one, then
// idle-syncs the local-only repos (which never converge across hosts). The peer list
// comes from the shared synckit host mesh, and origin tags the notifying peer so the
// converge pass can skip the redundant return hop; the whole pass is daemon-independent
// and self-heals when a peer is unreachable.
func Reconcile(ctx context.Context, st *state.State, origin string) ([]Result, error) {
	mesh, err := hostregistry.Mesh.Load()
	if err != nil {
		return nil, err
	}
	if len(mesh.Hosts) == 0 {
		log.Print("reconcile: WARNING the shared host mesh has no peers; converging nothing across hosts (run `synckitd host add` to migrate this host)")
	}
	converged, err := convergeRepos(ctx, st, mesh.Hosts, origin)
	if err != nil {
		return nil, err
	}
	local, err := Repos(ctx, st, localRepos(st))
	if err != nil {
		return nil, err
	}
	results := append(converged, local...)
	// The env pass rides the same tick and runs last, so a repo cloned above gets its
	// env files materialized in the same reconcile.
	return append(results, convergeEnv(ctx, st, mesh.Hosts, origin)...), nil
}

// localRepos returns the present local-only repos as the flat Repo view the
// reconcile sweep iterates.
func localRepos(st *state.State) []state.Repo {
	repos := make([]state.Repo, 0, len(st.LocalRepos))
	for _, e := range st.LocalRepos.Present() {
		repos = append(repos, state.Repo{Relpath: e.Value.Relpath, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly})
	}
	return repos
}

// Repos clones every absent repo and idle-syncs every present one among the given
// repos, holding the per-host reconcile flock for the whole pass. It does not
// pull-merge — it reconciles exactly the repos handed to it onto disk.
func Repos(ctx context.Context, st *state.State, repos []state.Repo) ([]Result, error) {
	var results []Result
	err := state.WithLock(ctx, func() error {
		dl, err := st.DefaultLocationExpanded()
		if err != nil {
			return err
		}
		tmpRoot := filepath.Join(dl, TmpDirName)
		defer func() { _ = os.RemoveAll(tmpRoot) }()

		results = make([]Result, len(repos))
		for i, repo := range repos {
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
	ctx, cancel := context.WithTimeout(ctx, time.Duration(st.Settings.RepoOpTimeout))
	defer cancel()

	abspath := repo.AbsPath(dl)
	if Present(abspath) {
		busy, err := idleSync(ctx, st, abspath)
		if busy {
			return Result{Relpath: repo.Relpath, Action: ActionBusy, Err: err}
		}
		return Result{Relpath: repo.Relpath, Action: ActionPresent, Err: err}
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
	// Busy is discarded: a fresh clone always looks recently active, and the clone did happen.
	_, err := idleSync(ctx, st, abspath)
	return Result{Relpath: repo.Relpath, Action: ActionCloned, Err: err}
}

// clone clones repo.Origin into a unique temp dir, verifies it is a colocated jj
// clone of the expected origin, then atomically renames it onto abspath. A
// pre-existing non-repo directory at abspath makes the rename fail loudly.
func clone(ctx context.Context, repo state.Repo, abspath, tmpRoot string) error {
	if err := os.MkdirAll(tmpRoot, 0o750); err != nil {
		return fmt.Errorf("create temp root %s: %w", tmpRoot, err)
	}
	parent, err := os.MkdirTemp(tmpRoot, "clone-*")
	if err != nil {
		return fmt.Errorf("create temp clone dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(parent) }()
	tmp := filepath.Join(parent, filepath.Base(repo.Relpath))

	if err := vcs.Clone(ctx, repo.Origin, tmp); err != nil {
		return err
	}
	if err := verifyClone(ctx, tmp, repo.Origin); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abspath), 0o750); err != nil {
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
// internal/sync so reconcile never duplicates the vcs flow. It reports whether
// that repo's sync was busy-gated alongside the joined error.
func idleSync(ctx context.Context, st *state.State, abspath string) (bool, error) {
	results, err := sync.Sync(ctx, st, abspath, "")
	if err != nil {
		return false, err
	}
	busy := false
	var errs []error
	for _, res := range results {
		if res.Outcome == sync.OutcomeBusy {
			busy = true
		}
		if res.Err != nil {
			errs = append(errs, res.Err)
		}
	}
	return busy, errors.Join(errs...)
}

// Present reports whether a repo checkout (jj or git) already exists at abspath.
func Present(abspath string) bool {
	for _, marker := range []string{".jj", ".git"} {
		if _, err := os.Stat(filepath.Join(abspath, marker)); err == nil {
			return true
		}
	}
	return false
}
