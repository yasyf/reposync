// Package watch runs the event-based watch daemon: it sets up fsnotify watches
// over each registered repo's VCS metadata directories and, on any change,
// debounces, resolves the origin trunk hash, dedupes by hash, and notifies every
// peer host to run a fast single-repo sync. The pure debounce/dedupe/notify core
// (engine) is separated from the fsnotify and ssh boundaries so it is driven
// directly in tests.
package watch

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/yasyf/reposync/internal/state"
)

// tmpDirName is the in-flight clone staging directory under default_location;
// events under it are clones landing mid-flight and must be ignored.
const tmpDirName = ".reposync-tmp"

// watchedRepo pairs a registered repo with its already-resolved absolute path.
type watchedRepo struct {
	repo state.Repo
	abs  string
}

// Watch blocks until ctx is canceled. It sets up fsnotify watches over every
// registered repo's VCS metadata directories, then runs the event loop: each
// event is mapped back to its repo by longest-prefix match and fed to the
// engine, which debounces, dedupes by trunk hash, and notifies peers.
func Watch(ctx context.Context, st *state.State) error {
	location, err := st.DefaultLocationExpanded()
	if err != nil {
		return err
	}

	watched := make([]watchedRepo, 0, len(st.Repos))
	for _, repo := range st.Repos {
		watched = append(watched, watchedRepo{repo: repo, abs: repo.AbsPath(location)})
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	tmpPrefix := filepath.Join(location, tmpDirName)
	for _, wr := range watched {
		for _, dir := range watchSet(wr.abs) {
			if !isDir(dir) {
				log.Printf("watch: %s: skip absent watch dir %s", wr.repo.Relpath, dir)
				continue
			}
			if err := w.Add(dir); err != nil {
				log.Printf("watch: %s: add watch %s: %v", wr.repo.Relpath, dir, err)
				continue
			}
		}
	}

	eng := newEngine(
		gitResolver{defaultLocation: location},
		sshNotifier{self: st.Self, defaultLocation: location},
		time.Duration(st.Settings.WatchDebounce),
		st.Hosts,
	)

	return runLoop(ctx, w, eng, watched, tmpPrefix)
}

// runLoop pumps fsnotify events into the engine until ctx is canceled. It is
// separate from Watch so the wiring stays thin and the engine carries the logic.
func runLoop(ctx context.Context, w *fsnotify.Watcher, eng *engine, watched []watchedRepo, tmpPrefix string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Printf("watch: fsnotify error: %v", err)
		case event, ok := <-w.Events:
			if !ok {
				return nil
			}
			if isUnderTmp(event.Name, tmpPrefix) {
				continue
			}
			repo, ok := matchRepo(event.Name, watched)
			if !ok {
				continue
			}
			eng.onEvent(ctx, repo)
		}
	}
}

// watchSet returns the directories to watch for a repo, by VCS kind. fsnotify on
// darwin uses kqueue, which watches directories (which survive create/delete/
// rename of children), never the not-yet-existent ref files themselves.
func watchSet(abs string) []string {
	git := filepath.Join(abs, ".git")
	originRemotes := filepath.Join(git, "refs", "remotes", "origin")
	if isDir(filepath.Join(abs, ".jj")) {
		return []string{
			filepath.Join(abs, ".jj", "repo", "op_heads", "heads"),
			originRemotes,
			git,
		}
	}
	return []string{
		originRemotes,
		git,
		filepath.Join(git, "logs", "refs", "remotes", "origin"),
	}
}

// matchRepo maps a changed path back to its repo by longest-prefix match against
// the registered repo roots, so a nested repo wins over its parent.
func matchRepo(path string, watched []watchedRepo) (state.Repo, bool) {
	best := -1
	var match state.Repo
	for _, wr := range watched {
		if !underRoot(path, wr.abs) {
			continue
		}
		if len(wr.abs) > best {
			best = len(wr.abs)
			match = wr.repo
		}
	}
	return match, best >= 0
}

// underRoot reports whether path is root itself or lives beneath it, comparing
// whole path segments so /a/b never matches /a/bc.
func underRoot(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// isUnderTmp reports whether path is the in-flight clone staging dir or beneath it.
func isUnderTmp(path, tmpPrefix string) bool {
	return underRoot(path, tmpPrefix)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
