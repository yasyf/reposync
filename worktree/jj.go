package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/yasyf/reposync/internal/vcs"
)

const jjWorkspaceTemplate = `"{\"name\":" ++ json(name) ++ ",\"root\":" ++ json(root) ++ ",\"parents\":" ++ json(target.parents().map(|c| c.commit_id())) ++ "}\n"`

type jjWorkspace struct {
	Name    string   `json:"name"`
	Root    string   `json:"root"`
	Parents []string `json:"parents"`
}

func jjWorkspaces(ctx context.Context, main Worktree) ([]Worktree, []Skip, error) {
	out, err := jjRead(ctx, main.Root, "workspace", "list", "-T", jjWorkspaceTemplate)
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var wts []Worktree
	var skips []Skip
	for {
		var ws jjWorkspace
		err := dec.Decode(&ws)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("decode jj workspace list: %w", err)
		}
		if len(ws.Parents) == 0 {
			return nil, nil, fmt.Errorf("jj workspace %q has no parent commit", ws.Name)
		}
		root, err := filepath.EvalSymlinks(ws.Root)
		if errors.Is(err, fs.ErrNotExist) {
			skips = append(skips, Skip{Path: ws.Root, Reason: "jj workspace missing"})
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("resolve jj workspace %s: %w", ws.Root, err)
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
			Name:      ws.Name,
			Kind:      KindJJWorkspace,
			Head:      ws.Parents[0],
		}
		if wt.Incarnation, err = inode(filepath.Join(root, ".jj", "working_copy")); err != nil {
			return nil, nil, err
		}
		wt.ID = worktreeID(wt.Origin, wt.Root, wt.Incarnation)
		wts = append(wts, wt)
	}
	return wts, skips, nil
}

const jjFilesTemplate = `self.files().map(|e| e.path() ++ "\0").join("")`

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
