package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yasyf/synckit/converge"
	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/rpc"
	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

const (
	// ActionEnvApplied means at least one of the repo's env files changed on disk.
	ActionEnvApplied = "env-applied"
	// ActionEnvClean means the repo's env files already matched the merged state.
	ActionEnvClean = "env-clean"
	// ActionEnvBusy means a file that would change was modified within the quiet window,
	// so the converge left the repo untouched and persisted nothing.
	ActionEnvBusy = "env-busy"
)

// envFetchTimeout bounds one peer's env get_state exchange.
const envFetchTimeout = 60 * time.Second

// errUnknownMethod marks a peer whose rpc-serve predates env sync; it answers
// env.get_state with an unknown-method error and degrades to a skipped peer.
var errUnknownMethod = errors.New("peer does not serve env.get_state")

// envPeerStatus is the process-lived tracker the env converge logs unreachable and
// old-version peers against, so an outage warns once rather than every pass.
var envPeerStatus = converge.NewPeerStatus()

// envFetch is the peer env fetcher the reconcile env pass drives; a var so a test can
// inject a fake without a real ssh hop.
var envFetch envFetcher = sshEnvFetcher{}

// envFetcher reads a peer's stamped env state for the requested origins, read-only.
type envFetcher interface {
	FetchEnv(ctx context.Context, peer string, origins []string) (map[string]env.RepoState, error)
}

// sshEnvFetcher fetches a peer's env state over the same ssh-stdio rpc-serve bridge the
// repo registry uses, issuing a raw env.get_state request the typed client does not
// know. It is the pull side of pull-merge and never writes to the peer.
type sshEnvFetcher struct{}

// FetchEnv issues env.get_state to peer for origins and returns the served RepoState per
// origin. A peer that does not know the method returns errUnknownMethod.
func (sshEnvFetcher) FetchEnv(ctx context.Context, peer string, origins []string) (map[string]env.RepoState, error) {
	ctx, cancel := context.WithTimeout(ctx, envFetchTimeout)
	defer cancel()
	tx := syncservice.SSHStdio(peer, "reposync rpc-serve")
	defer func() { _ = tx.Close() }()

	resp, err := tx.Do(ctx, &rpc.Request{Method: env.MethodGetState, Params: map[string]any{"origins": origins}})
	if err != nil {
		return nil, fmt.Errorf("env get_state from %s: %w", peer, err)
	}
	if !resp.OK {
		if strings.Contains(resp.Error, "unknown method") {
			return nil, fmt.Errorf("%s: %w", peer, errUnknownMethod)
		}
		return nil, fmt.Errorf("env get_state from %s: %s", peer, resp.Error)
	}
	var payload struct {
		Repos map[string]env.RepoState `json:"repos"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		return nil, fmt.Errorf("decode env get_state from %s: %w", peer, err)
	}
	return payload.Repos, nil
}

// convergeEnv runs the reconcile env pass: fetch every peer's stamped env state, then
// key-level 3-way merge each eligible repo's root .env files under the flock. It is
// best-effort — a pass-level setup failure is logged and yields no results — so the git
// converge that precedes it is never blocked by env sync.
func convergeEnv(ctx context.Context, st *state.State, peers []string, origin string) []Result {
	return convergeEnvWith(ctx, st, envFetch, envPeerStatus, peers, origin)
}

// convergeEnvWith is convergeEnv with the fetcher and transition tracker injected so a
// test can drive the pull-merge against a fake peer. Unlike the repo converge it does
// NOT skip the notifying origin peer: for env the notifying host is the only holder of
// the new content, and termination comes from the apply-stable digest, not an origin
// skip. The trailing origin arg is therefore accepted for symmetry but never consulted.
func convergeEnvWith(ctx context.Context, st *state.State, f envFetcher, status *converge.PeerStatus, peers []string, _ string) []Result {
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		slog.ErrorContext(ctx, "env converge: resolve default location", "err", err)
		return nil
	}
	configDir, err := state.Dir()
	if err != nil {
		slog.ErrorContext(ctx, "env converge: resolve config dir", "err", err)
		return nil
	}
	eligible := eligibleEnvRepos(st, dl)
	if len(eligible) == 0 {
		return nil
	}
	origins := make([]string, len(eligible))
	for i, r := range eligible {
		origins[i] = r.Origin
	}
	// Fetch every peer BEFORE the flock so a slow peer never holds this host's lock.
	peerStates := fetchEnvPeers(ctx, f, status, peers, origins)

	var results []Result
	if err := state.WithLock(ctx, func() error {
		results = make([]Result, len(eligible))
		for i, repo := range eligible {
			results[i] = convergeEnvRepo(ctx, dl, configDir, repo, peerStates[repo.Origin])
		}
		return nil
	}); err != nil {
		slog.ErrorContext(ctx, "env converge: lock", "err", err)
		return nil
	}
	return results
}

// eligibleEnvRepos returns the present-on-disk propagating repos that sync env files: an
// origin-bearing, non-local-only repo that has not opted out via NoEnvSync.
func eligibleEnvRepos(st *state.State, dl string) []state.Repo {
	var out []state.Repo
	for _, r := range st.PropagatingRepos() {
		if r.LocalOnly || r.NoEnvSync {
			continue
		}
		if !Present(r.AbsPath(dl)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// fetchEnvPeers fetches and validates every peer's env state for origins, returning the
// per-origin list of peer RepoStates to merge. A peer that is unreachable, old-version,
// or serves an invalid payload is logged and skipped; the notifying origin peer is NOT
// skipped.
func fetchEnvPeers(ctx context.Context, f envFetcher, status *converge.PeerStatus, peers, origins []string) map[string][]env.RepoState {
	requested := make(map[string]bool, len(origins))
	for _, o := range origins {
		requested[o] = true
	}
	byOrigin := make(map[string][]env.RepoState)
	for _, peer := range peers {
		payload, err := f.FetchEnv(ctx, peer, origins)
		if err != nil {
			if status.Down(peer) {
				slog.WarnContext(ctx, "env converge: peer skipped; suppressing until recovery", "peer", peer, "err", err)
			}
			continue
		}
		if _, recovered := status.Up(peer); recovered {
			slog.InfoContext(ctx, "env converge: peer recovered", "peer", peer)
		}
		if err := validatePeerEnv(requested, payload); err != nil {
			slog.WarnContext(ctx, "env converge: rejecting peer payload", "peer", peer, "err", err)
			continue
		}
		for o, rs := range payload {
			byOrigin[o] = append(byOrigin[o], rs)
		}
	}
	return byOrigin
}

// validatePeerEnv rejects a peer's ENTIRE payload on any wire violation: an origin the
// pass did not request, a file name that fails ValidateFileName, an entry whose key or
// value could inject an extra line, a stamp outside [0, now+MaxStampSkew], or a count
// or aggregate size over the wire caps. Rejecting whole avoids a partially-applied
// malicious payload and keeps a poisoned stamp out of the local sidecar entirely.
func validatePeerEnv(requested map[string]bool, payload map[string]env.RepoState) error {
	ceiling := cregistry.UnixMicros(time.Now().Add(env.MaxStampSkew))
	for origin, rs := range payload {
		if !requested[origin] {
			return fmt.Errorf("peer served unrequested origin %q", origin)
		}
		if len(rs) > env.MaxWireFiles {
			return fmt.Errorf("origin %q served %d env files, over the %d cap", origin, len(rs), env.MaxWireFiles)
		}
		for name, reg := range rs {
			if err := env.ValidateFileName(name); err != nil {
				return err
			}
			if len(reg) > env.MaxWireKeys {
				return fmt.Errorf("env file %q served %d entries, over the %d cap", name, len(reg), env.MaxWireKeys)
			}
			size := 0
			for key, e := range reg {
				if !env.ValidKey(key) {
					return fmt.Errorf("invalid env key %q in %q", key, name)
				}
				if !env.ValidValue(e.Value) {
					return fmt.Errorf("env value for %q in %q contains a newline", key, name)
				}
				if e.Added < 0 || e.Removed < 0 || e.Added > ceiling || e.Removed > ceiling {
					return fmt.Errorf("env entry %q in %q has an out-of-range stamp", key, name)
				}
				size += len(key) + len(e.Value)
			}
			if size > env.MaxFileSize {
				return fmt.Errorf("env file %q served %d aggregate bytes, over the %d cap", name, size, env.MaxFileSize)
			}
		}
	}
	return nil
}

// convergeEnvRepo merges one repo's root .env files: build the shared local state, join
// with the peers' states, drop names this host must not sync, gate on the quiet window,
// apply, prune tombstone-only names this host never held, and persist the sidecar as
// the next merge base.
func convergeEnvRepo(ctx context.Context, dl, configDir string, repo state.Repo, peerStates []env.RepoState) Result {
	abspath := repo.AbsPath(dl)
	sidecarPath := env.SidecarPath(configDir, repo.Origin)
	local, err := LocalEnvState(ctx, abspath, sidecarPath, repo.Origin)
	if err != nil {
		return Result{Relpath: repo.Relpath, Err: err}
	}
	// Reload the persisted sidecar as the skip-write reference: LocalEnvState folded a
	// copy, and comparing merged against it lets a no-op pass skip the sidecar rewrite.
	sc, err := env.LoadSidecar(sidecarPath, repo.Origin)
	if err != nil {
		return Result{Relpath: repo.Relpath, Err: err}
	}
	merged := env.Merge(append([]env.RepoState{local}, peerStates...)...)
	if err := dropUnsyncable(ctx, abspath, merged); err != nil {
		return Result{Relpath: repo.Relpath, Err: err}
	}
	busy, err := envBusy(abspath, local, merged)
	if err != nil {
		return Result{Relpath: repo.Relpath, Err: err}
	}
	if busy {
		return Result{Relpath: repo.Relpath, Action: ActionEnvBusy}
	}
	changed, err := applyEnvFiles(abspath, merged)
	if err != nil {
		return Result{Relpath: repo.Relpath, Err: err}
	}
	if err := dropTombstoneOnlyAbsent(abspath, sc.Files, merged); err != nil {
		return Result{Relpath: repo.Relpath, Err: err}
	}
	if changed || !sameRepoState(sc.Files, merged) {
		if err := (env.Sidecar{Origin: repo.Origin, Files: merged}).Save(sidecarPath); err != nil {
			return Result{Relpath: repo.Relpath, Err: err}
		}
	}
	if changed {
		return Result{Relpath: repo.Relpath, Action: ActionEnvApplied}
	}
	return Result{Relpath: repo.Relpath, Action: ActionEnvClean}
}

// LocalEnvState is this host's syncable env state for the repo at root: scan the root
// .env files, observe them against the origin's sidecar, then drop every name it must
// not sync — locally git-tracked, exempt (symlink or oversized), or failing
// ValidateFileName. Both the converge pass's local-state step and the rpc serve handler
// call it, so a file that became git-tracked after being synced is neither merged nor
// served.
func LocalEnvState(ctx context.Context, root, sidecarPath, origin string) (env.RepoState, error) {
	names, err := env.ScanNames(root)
	if err != nil {
		return nil, err
	}
	tracked, err := vcs.TrackedNames(ctx, root, names)
	if err != nil {
		return nil, fmt.Errorf("list tracked env files in %s: %w", root, err)
	}
	sc, err := env.LoadSidecar(sidecarPath, origin)
	if err != nil {
		return nil, err
	}
	local, err := env.Observe(sc, root, untracked(names, tracked))
	if err != nil {
		return nil, err
	}
	if err := dropUnsyncable(ctx, root, local); err != nil {
		return nil, err
	}
	return local, nil
}

// untracked returns the names git does not track, the set this host observes and serves.
func untracked(names []string, tracked map[string]bool) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !tracked[n] {
			out = append(out, n)
		}
	}
	return out
}

// dropUnsyncable removes from merged every file this host must not sync: a git-tracked
// name (git already carries it), an exempt path (symlink or oversized — a deliberate
// local arrangement), or a name that fails ValidateFileName. The persisted sidecar must
// describe only files this host actually syncs, and this drop is what finally purges a
// now-tracked or now-exempt name that Observe carried through.
func dropUnsyncable(ctx context.Context, root string, merged env.RepoState) error {
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	tracked, err := vcs.TrackedNames(ctx, root, names)
	if err != nil {
		return fmt.Errorf("list tracked env files in %s: %w", root, err)
	}
	for name := range merged {
		if env.ValidateFileName(name) != nil || tracked[name] {
			delete(merged, name)
			continue
		}
		exempt, err := env.Exempt(root, name)
		if err != nil {
			return err
		}
		if exempt {
			delete(merged, name)
		}
	}
	return nil
}

// dropTombstoneOnlyAbsent removes from merged every file name with no present keys, no
// backing regular file, and no entry in base (the loaded sidecar): peer-injected
// tombstone spam never persists, while a locally deleted file's name stays in base and
// keeps propagating its deletion until TombstoneTTL GC.
func dropTombstoneOnlyAbsent(root string, base, merged env.RepoState) error {
	for name, reg := range merged {
		if _, held := base[name]; held {
			continue
		}
		if len(reg.Present()) > 0 {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			if os.IsNotExist(err) {
				delete(merged, name)
				continue
			}
			return fmt.Errorf("lstat env file %s: %w", filepath.Join(root, name), err)
		}
		if !info.Mode().IsRegular() {
			delete(merged, name)
		}
	}
	return nil
}

// envBusy reports whether applying merged would rewrite an existing file modified within
// the quiet window — a concurrent local edit the converge must not race. A file whose
// merged content matches its observed local content will not be rewritten, so it never
// gates; a file that would be created has no edit to race and never gates either.
func envBusy(root string, local, merged env.RepoState) (bool, error) {
	now := time.Now()
	for name, reg := range merged {
		if !presentDiffers(local[name], reg) {
			continue
		}
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("stat env file %s: %w", filepath.Join(root, name), err)
		}
		if now.Sub(info.ModTime()) < env.QuietWindow {
			return true, nil
		}
	}
	return false, nil
}

// applyEnvFiles reconciles every merged file to disk and reports whether any changed.
func applyEnvFiles(root string, merged env.RepoState) (bool, error) {
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	changed := false
	for _, name := range names {
		wrote, err := env.Apply(filepath.Join(root, name), merged[name])
		if err != nil {
			return changed, err
		}
		if wrote {
			changed = true
		}
	}
	return changed, nil
}

// presentDiffers reports whether a and b hold different present key/value content,
// ignoring stamps and tombstones.
func presentDiffers(a, b env.FileMap) bool {
	return !maps.Equal(presentValues(a), presentValues(b))
}

// presentValues projects a registry to its present key/value map.
func presentValues(reg env.FileMap) map[string]string {
	m := make(map[string]string, len(reg))
	for k, e := range reg {
		if e.Present() {
			m[k] = e.Value
		}
	}
	return m
}

// sameRepoState reports whether two RepoStates are identical entry-for-entry, so an
// unchanged merge skips the sidecar write.
func sameRepoState(a, b env.RepoState) bool {
	return maps.EqualFunc(a, b, func(x, y env.FileMap) bool { return maps.Equal(x, y) })
}
