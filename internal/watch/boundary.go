package watch

import (
	"context"
	"fmt"

	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// gitResolver resolves origin/<trunk> through git rev-parse via the vcs layer,
// never reading a ref file directly. defaultLocation is the already-expanded
// absolute path the repo's relpath is joined onto.
type gitResolver struct {
	defaultLocation string
}

func (r gitResolver) Resolve(ctx context.Context, repo state.Repo) (string, error) {
	abs := repo.AbsPath(r.defaultLocation)
	opened, err := vcs.Open(abs, repo.Trunk)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", abs, err)
	}
	return opened.TrunkHash(ctx)
}

// peerRunner runs a command on a peer over ssh; host.NewExecRunner satisfies it.
type peerRunner interface {
	SSH(ctx context.Context, target, remoteCmd string) (string, error)
}

// rpcNotifier notifies a peer by ssh-ing it to trigger a single-repo sync on the
// peer's resident daemon over its RPC socket, rather than spawning a full remote
// sync process. The relpath is host-agnostic (the peer resolves its own absolute
// path), and self is passed as the --origin provenance tag so the peer can
// suppress the redundant return hop.
type rpcNotifier struct {
	self   string
	runner peerRunner
}

func (n rpcNotifier) Notify(ctx context.Context, peer string, repo state.Repo) error {
	cmd := fmt.Sprintf("reposync rpc sync --relpath %s --origin %s", host.ShellQuote(repo.Relpath), host.ShellQuote(n.self))
	if _, err := n.runner.SSH(ctx, peer, cmd); err != nil {
		return fmt.Errorf("ssh %s: %w", peer, err)
	}
	return nil
}
