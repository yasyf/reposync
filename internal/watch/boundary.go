package watch

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// brewShellenv sources Homebrew onto a non-interactive ssh session's PATH so the
// remote reposync binary is found. /opt/homebrew is the Apple Silicon prefix.
const brewShellenv = `eval "$(/opt/homebrew/bin/brew shellenv)"`

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

// sshNotifier notifies a peer by ssh-ing it to run a fast single-repo sync. self
// is this host's identity, passed as the --origin provenance tag so the peer can
// suppress the redundant return hop.
type sshNotifier struct {
	self            string
	defaultLocation string
}

func (n sshNotifier) Notify(ctx context.Context, peer string, repo state.Repo) error {
	abs := repo.AbsPath(n.defaultLocation)
	remote := fmt.Sprintf("%s && reposync sync --repo %s --origin %s", brewShellenv, abs, n.self)
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		peer, remote)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s: %w: %s", peer, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
