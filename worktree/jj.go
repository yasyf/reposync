package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/yasyf/reposync/internal/vcs"
)

const jjWorkspaceTemplate = `name ++ "\t" ++ root ++ "\t" ++ target.parents().map(|c| c.commit_id()).join(",") ++ "\n"`

func jjWorkspaces(ctx context.Context, main Worktree) ([]Worktree, []Skip, error) {
	out, err := jjRead(ctx, main.Root, "workspace", "list", "-T", jjWorkspaceTemplate)
	if err != nil {
		return nil, nil, err
	}
	var wts []Worktree
	var skips []Skip
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, nil, fmt.Errorf("jj workspace list line %q", line)
		}
		name, rawRoot, parents := fields[0], fields[1], fields[2]
		root, err := filepath.EvalSymlinks(rawRoot)
		if errors.Is(err, fs.ErrNotExist) {
			skips = append(skips, Skip{Path: rawRoot, Reason: "jj workspace missing"})
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("resolve jj workspace %s: %w", rawRoot, err)
		}
		if root == main.Root {
			continue
		}
		wt := Worktree{
			Origin:    main.Origin,
			Relpath:   main.Relpath,
			Trunk:     main.Trunk,
			Root:      root,
			CommonDir: main.CommonDir,
			Name:      name,
			Kind:      KindJJWorkspace,
		}
		wt.Head, _, _ = strings.Cut(parents, ",")
		if wt.Incarnation, err = inode(filepath.Join(root, ".jj", "working_copy")); err != nil {
			return nil, nil, err
		}
		wt.ID = worktreeID(wt.Origin, wt.Root, wt.Incarnation)
		wts = append(wts, wt)
	}
	return wts, skips, nil
}

func jjRead(ctx context.Context, root string, args ...string) (string, error) {
	var out bytes.Buffer
	err := vcs.Exec(ctx, vcs.Cmd{
		Dir:    root,
		Name:   "jj",
		Args:   append([]string{"--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never", "-R", root}, args...),
		Env:    vcs.ReadOnlyGitEnv(),
		Stdout: &out,
	})
	if err != nil {
		return "", fmt.Errorf("jj %s in %s: %w", strings.Join(args, " "), root, err)
	}
	return out.String(), nil
}
