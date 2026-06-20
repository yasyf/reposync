// Package watch runs the event-based watch daemon: it subscribes via watchman to
// each registered repo's VCS metadata directories and, on any change, debounces,
// resolves the origin trunk hash, dedupes by hash, and notifies every peer host to
// run a fast single-repo sync. It also serves the RPC socket so peers can trigger a
// local sync or reconcile. The pure debounce/dedupe/notify core (engine) is kept
// separate from the watchman and ssh boundaries so it is driven directly in tests.
package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/rpc"
	"github.com/yasyf/reposync/internal/state"
)

// Watch blocks until ctx is canceled. It subscribes via watchman to every
// registered repo's VCS metadata directories and binds the RPC socket, then runs
// both concurrently: each subscription update is mapped back to its repo by
// subscription name and fed to the engine (debounce, dedupe by trunk hash, notify
// peers), while the RPC server lets peers trigger a local sync or reconcile. The
// first of the two to fail cancels the other.
func Watch(ctx context.Context, st *state.State) error {
	if _, err := exec.LookPath("watchman"); err != nil {
		return fmt.Errorf("watchman is required by the watch daemon but was not found: %w", err)
	}

	location, err := st.DefaultLocationExpanded()
	if err != nil {
		return err
	}

	eng := newEngine(
		gitResolver{defaultLocation: location},
		rpcNotifier{self: st.Self, runner: host.NewExecRunner()},
		time.Duration(st.Settings.WatchDebounce),
		st.Hosts,
	)

	wm, err := dialWatchman(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = wm.close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	subs := map[string]state.Repo{}
	dispatch := func(pdu map[string]json.RawMessage) {
		var name string
		if raw, ok := pdu["subscription"]; ok {
			_ = json.Unmarshal(raw, &name)
		}
		if repo, ok := subs[name]; ok {
			eng.onEvent(ctx, repo)
		}
	}

	for _, repo := range st.Repos {
		abs := repo.AbsPath(location)
		for i, dir := range watchSet(abs) {
			if !isDir(dir) {
				log.Printf("watch: %s: skip absent watch dir %s", repo.Relpath, dir)
				continue
			}
			// watchman resolves the watch root with strict case-sensitive rules and
			// rejects a symlinked path (e.g. macOS /tmp -> /private/tmp), so hand it
			// the real path.
			realPath, err := filepath.EvalSymlinks(dir)
			if err != nil {
				log.Printf("watch: %s: resolve watch dir %s: %v", repo.Relpath, dir, err)
				continue
			}
			name := fmt.Sprintf("reposync:%s:%d", repo.Relpath, i)
			subs[name] = repo
			if err := wm.subscribe(realPath, name, dispatch); err != nil {
				delete(subs, name)
				log.Printf("watch: %s: %v", repo.Relpath, err)
			}
		}
	}

	ln, err := listenRPC()
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()

	errc := make(chan error, 2)
	go func() { errc <- wm.runSubscriptions(ctx, dispatch) }()
	go func() { errc <- rpc.NewServer(state.Load).Serve(ctx, ln) }()
	err = <-errc
	cancel()
	<-errc
	return err
}

// listenRPC binds the daemon's RPC unix socket, first unlinking any stale socket
// left behind by a crashed daemon so a launchd relaunch does not fail with
// EADDRINUSE. The returned listener unlinks the socket again on Close.
func listenRPC() (net.Listener, error) {
	dir, err := state.Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	sock, err := state.SockPath()
	if err != nil {
		return nil, err
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale rpc socket %s: %w", sock, err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listen on rpc socket %s: %w", sock, err)
	}
	return ln, nil
}

// watchSet returns the VCS metadata leaf directories to watch for a repo, by kind.
// Each is a small directory holding origin trunk refs (and the jj op log), so it is
// cheap to watch recursively — unlike the repo root, which watchman ignores as a
// VCS dir, or .git, whose object store would be crawled. A loose-ref fetch always
// touches one of these; the rare packed-refs case is caught by the reconcile tick.
func watchSet(abs string) []string {
	git := filepath.Join(abs, ".git")
	originRefs := filepath.Join(git, "refs", "remotes", "origin")
	originLogs := filepath.Join(git, "logs", "refs", "remotes", "origin")
	if isDir(filepath.Join(abs, ".jj")) {
		return []string{
			filepath.Join(abs, ".jj", "repo", "op_heads", "heads"),
			originRefs,
			originLogs,
		}
	}
	return []string{
		originRefs,
		originLogs,
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
