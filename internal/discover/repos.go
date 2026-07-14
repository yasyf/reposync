package discover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// Repos scans st's expanded default location one level deep, classifying each
// child directory and annotating whether it is already tracked.
func Repos(ctx context.Context, st *state.State) (RepoResult, error) {
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return RepoResult{}, fmt.Errorf("expand default location: %w", err)
	}
	entries, err := os.ReadDir(dl)
	if os.IsNotExist(err) {
		return RepoResult{}, nil
	}
	if err != nil {
		return RepoResult{}, fmt.Errorf("read default location %s: %w", dl, err)
	}

	var result RepoResult
	for _, entry := range entries {
		name := entry.Name()
		if name[0] == '.' || name == reconcile.TmpDirName {
			continue
		}
		abs := filepath.Join(dl, name)
		if !isDir(entry, abs) {
			continue
		}
		candidate, note, ok := classify(ctx, st, name, abs)
		if note != nil {
			result.Skipped = append(result.Skipped, *note)
			continue
		}
		if ok {
			result.Candidates = append(result.Candidates, candidate)
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].Relpath < result.Candidates[j].Relpath
	})
	return result, nil
}

// classify opens abs as a repo and builds its Candidate. A non-repo directory
// is silently dropped (ok=false, note=nil); a probe failure surfaces a SkipNote.
func classify(ctx context.Context, st *state.State, name, abs string) (Candidate, *SkipNote, bool) {
	r, err := vcs.Open(abs, "")
	if errors.Is(err, vcs.ErrNotARepo) {
		return Candidate{}, nil, false
	}
	if err != nil {
		return Candidate{}, &SkipNote{Name: name, Reason: err.Error()}, false
	}
	origin, err := r.Origin(ctx)
	if errors.Is(err, vcs.ErrNoOrigin) {
		origin = ""
	} else if err != nil {
		return Candidate{}, &SkipNote{Name: name, Reason: err.Error()}, false
	}
	isTracked, noEnvSync := tracked(st, name, origin)
	return Candidate{
		Relpath:   name,
		AbsPath:   abs,
		Kind:      r.Kind(),
		Origin:    origin,
		LocalOnly: origin == "",
		Tracked:   isTracked,
		NoEnvSync: noEnvSync,
	}, nil, true
}

// tracked reports whether st already registers this repo — matching on origin when
// present, otherwise on relpath against the local-only registry — and, when tracked,
// whether it has opted out of env-file sync.
func tracked(st *state.State, name, origin string) (isTracked, noEnvSync bool) {
	if origin != "" {
		r, ok := st.FindRepoByOrigin(origin)
		return ok, ok && r.NoEnvSync
	}
	e, ok := st.LocalRepos[name]
	present := ok && e.Present()
	return present, present && e.Value.NoEnvSync
}

// isDir reports whether the entry resolves to a directory, following a symlink
// to its target.
func isDir(entry os.DirEntry, abs string) bool {
	if entry.Type()&os.ModeSymlink == 0 {
		return entry.IsDir()
	}
	info, err := os.Stat(abs)
	return err == nil && info.IsDir()
}
