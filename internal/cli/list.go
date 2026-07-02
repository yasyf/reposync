package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// watchItems builds the per-repo watch items for both registries: propagating repos
// keyed by origin and local-only repos keyed by relpath. It never drops a repo —
// an uncloned or unreadable repo reports an empty fingerprint, logging the cause to
// errw, so synckitd keeps the subscription and converges once the repo lands. The two
// id namespaces (origin vs relpath) stay distinct so a propagating repo and a
// local-only one never collide.
func watchItems(ctx context.Context, errw io.Writer, st *state.State, dl string) []syncservice.WatchItem {
	items := make([]syncservice.WatchItem, 0, len(st.Repos)+len(st.LocalRepos))
	for origin, e := range st.Repos.Present() {
		items = append(items, watchItem(ctx, errw, origin, state.Repo{Relpath: e.Value.Relpath, Origin: origin, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly}, dl))
	}
	for relpath, e := range st.LocalRepos.Present() {
		items = append(items, watchItem(ctx, errw, relpath, state.Repo{Relpath: e.Value.Relpath, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly}, dl))
	}
	return items
}

func watchItem(ctx context.Context, errw io.Writer, id string, repo state.Repo, dl string) syncservice.WatchItem {
	abs := repo.AbsPath(dl)
	return syncservice.WatchItem{
		ID:          id,
		WatchDirs:   vcs.WatchPaths(abs),
		Fingerprint: trunkHash(ctx, errw, abs, repo.Trunk),
	}
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
