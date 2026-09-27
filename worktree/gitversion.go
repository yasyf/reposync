package worktree

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/yasyf/reposync/internal/vcs"
)

var minGit = [2]int{2, 44}

func requireGit(ctx context.Context) error {
	var out bytes.Buffer
	if err := vcs.Exec(ctx, vcs.Cmd{Name: "git", Args: []string{"version"}, Stdout: &out}); err != nil {
		return fmt.Errorf("git version: %w", err)
	}
	return checkGitVersion(out.String())
}

func checkGitVersion(out string) error {
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != "git" || f[1] != "version" {
		return fmt.Errorf("unrecognized git version output %q", out)
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("unrecognized git version %q", f[2])
	}
	var have [2]int
	for i := range have {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return fmt.Errorf("unrecognized git version %q: %w", f[2], err)
		}
		have[i] = n
	}
	if have[0] < minGit[0] || have[0] == minGit[0] && have[1] < minGit[1] {
		return &GitVersionError{Have: f[2], Want: fmt.Sprintf("%d.%d", minGit[0], minGit[1])}
	}
	return nil
}
