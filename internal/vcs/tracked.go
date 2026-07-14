package vcs

import (
	"context"
	"strings"
)

// TrackedNames reports which of names git tracks in the repo at root, running
// `git ls-files -z -- <names...>` (which reads the git index, so it works in a
// colocated jj checkout). names are repo-root-relative; an empty slice returns an
// empty set without invoking git.
func TrackedNames(ctx context.Context, root string, names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return map[string]bool{}, nil
	}
	args := append([]string{"-C", root, "ls-files", "-z", "--"}, names...)
	out, err := run(ctx, root, "git", args...)
	if err != nil {
		return nil, err
	}
	tracked := make(map[string]bool)
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			tracked[name] = true
		}
	}
	return tracked, nil
}
