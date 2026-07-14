package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// watchItems builds the per-repo watch items for both registries: propagating repos
// keyed by origin and local-only repos keyed by relpath. It never drops a repo —
// an uncloned or unreadable repo reports empty fingerprint components, logging the
// cause to errw, so synckitd keeps the subscription and converges once the repo lands. The two
// id namespaces (origin vs relpath) stay distinct so a propagating repo and a
// local-only one never collide.
func watchItems(ctx context.Context, errw io.Writer, st *state.State, dl string) []syncservice.WatchItem {
	items := make([]syncservice.WatchItem, 0, len(st.Repos)+len(st.LocalRepos))
	for origin, e := range st.Repos.Present() {
		items = append(items, watchItem(ctx, errw, origin, state.Repo{Relpath: e.Value.Relpath, Origin: origin, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly, NoEnvSync: e.Value.NoEnvSync}, dl))
	}
	for relpath, e := range st.LocalRepos.Present() {
		items = append(items, watchItem(ctx, errw, relpath, state.Repo{Relpath: e.Value.Relpath, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly, NoEnvSync: e.Value.NoEnvSync}, dl))
	}
	return items
}

func watchItem(ctx context.Context, errw io.Writer, id string, repo state.Repo, dl string) syncservice.WatchItem {
	abs := repo.AbsPath(dl)
	reason := busyReason(errw, abs)
	watchDirs := vcs.WatchPaths(abs)
	envDigest := ""
	if envEligible(repo) {
		envPaths, err := envWatchPaths(ctx, abs)
		if err != nil {
			_, _ = fmt.Fprintf(errw, "reposync list: %s: %v\n", abs, err)
			envPaths = staticEnvWatchPaths(abs)
		}
		watchDirs = append(watchDirs, envPaths...)

		envDigest, err = localEnvDigest(ctx, abs, repo.Origin)
		if err != nil {
			_, _ = fmt.Fprintf(errw, "reposync list: %s: %v\n", abs, err)
			envDigest = ""
		}
	}
	return syncservice.WatchItem{
		ID:          id,
		WatchDirs:   watchDirs,
		Fingerprint: trunkHash(ctx, errw, abs, repo.Trunk) + "\n" + envDigest,
		Busy:        reason != "",
		BusyReason:  reason,
	}
}

func envEligible(repo state.Repo) bool {
	return repo.Origin != "" && !repo.LocalOnly && !repo.NoEnvSync
}

func envWatchPaths(ctx context.Context, abs string) ([]string, error) {
	names, err := env.ScanNames(abs)
	if err != nil {
		return nil, err
	}
	tracked, err := vcs.TrackedNames(ctx, abs, names)
	if err != nil {
		return nil, fmt.Errorf("list tracked env files: %w", err)
	}

	watchNames := map[string]bool{
		".env":       true,
		".env.local": true,
	}
	for _, name := range names {
		if !tracked[name] {
			watchNames[name] = true
		}
	}
	names = names[:0]
	for name := range watchNames {
		names = append(names, name)
	}
	sort.Strings(names)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(abs, name))
	}
	return paths, nil
}

func staticEnvWatchPaths(abs string) []string {
	return []string{filepath.Join(abs, ".env"), filepath.Join(abs, ".env.local")}
}

func localEnvDigest(ctx context.Context, abs, origin string) (string, error) {
	configDir, err := state.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	local, err := reconcile.LocalEnvState(ctx, abs, env.SidecarPath(configDir, origin), origin)
	if err != nil {
		return "", fmt.Errorf("read local env state: %w", err)
	}
	return env.Digest(local), nil
}

// busyReason probes for a live git/jj operation at abs by lock-marker presence, the
// busy signal synckitd's watch gate defers on. It returns "" (never an error) when
// the probe fails, logging the cause so a probe failure never drops the item.
func busyReason(errw io.Writer, abs string) string {
	reason, err := vcs.OpInProgress(abs)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "reposync list: %s: %v\n", abs, err)
		return ""
	}
	return reason
}

// trunkHash resolves the upstream trunk commit hash through the vcs layer, the
// repo's change fingerprint. It returns "" (never an error) when the repo is not yet
// cloned or the hash cannot be read, logging the cause so synckitd keeps watching.
func trunkHash(ctx context.Context, errw io.Writer, abs, trunk string) string {
	opened, err := vcs.Open(abs, trunk)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "reposync list: %s: %v\n", abs, err)
		return ""
	}
	hash, err := opened.TrunkHash(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "reposync list: %s: %v\n", abs, err)
		return ""
	}
	return hash
}
