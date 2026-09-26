package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/yasyf/reposync/internal/vcs"
	"github.com/yasyf/reposync/registry"
)

// Kind is how a worktree is checked out.
type Kind string

const (
	// KindGit is a main or linked git worktree.
	KindGit Kind = "git"
	// KindJJColocated is the default workspace of a colocated jj repo; its git
	// HEAD is jj's @-.
	KindJJColocated Kind = "jj-colocated"
	// KindJJWorkspace is a secondary jj workspace: it has no .git and is read
	// through the repository's git store.
	KindJJWorkspace Kind = "jj-workspace"
)

// Worktree is one checkout of a registered repository.
type Worktree struct {
	// ID is hex(sha256(origin NUL root NUL incarnation))[:32].
	ID      string `json:"id"`
	Origin  string `json:"origin"`
	Relpath string `json:"relpath"`
	Trunk   string `json:"trunk"`
	// Root is the symlink-resolved worktree root.
	Root string `json:"root"`
	// GitDir is the per-worktree admin dir (CommonDir for a main worktree); ""
	// for a jj workspace.
	GitDir    string `json:"git_dir"`
	CommonDir string `json:"common_dir"`
	// Name is "" for a main worktree, the admin dir basename for a linked one,
	// and the workspace name for a jj workspace.
	Name   string `json:"name"`
	Kind   Kind   `json:"kind"`
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head"`
	// Incarnation is the inode of the per-worktree admin dir (jj:
	// <root>/.jj/working_copy), so a worktree recreated at the same path gets a
	// new ID.
	Incarnation uint64 `json:"incarnation"`
	Locked      bool   `json:"locked,omitempty"`
}

// Skip is a registered checkout or worktree Discover could not use.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type listedWorktree struct {
	path     string
	head     string
	branch   string
	bare     bool
	locked   bool
	prunable bool
}

// Discover lists every worktree of every propagating registered repository:
// the main and linked git worktrees (bare and prunable ones skipped) and, for a
// colocated jj repo, its secondary jj workspaces. A missing checkout is a Skip,
// not an error. Discovery only reads the repositories.
func Discover(ctx context.Context, reg registry.Registry) ([]Worktree, []Skip, error) {
	var wts []Worktree
	var skips []Skip
	seen := map[string]bool{}
	for _, repo := range reg.Repos {
		if repo.LocalOnly || repo.Origin == "" {
			continue
		}
		found, skipped, err := discoverRepo(ctx, repo)
		if err != nil {
			return nil, nil, fmt.Errorf("discover %s: %w", repo.Relpath, err)
		}
		skips = append(skips, skipped...)
		for _, wt := range found {
			if !seen[wt.Root] {
				seen[wt.Root] = true
				wts = append(wts, wt)
			}
		}
	}
	return wts, skips, nil
}

// Locate returns the worktree whose symlink-resolved Root most deeply contains
// path.
func Locate(wts []Worktree, path string) (Worktree, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = filepath.Clean(path)
	}
	var best Worktree
	found := false
	for _, wt := range wts {
		inside := resolved == wt.Root || strings.HasPrefix(resolved, wt.Root+string(filepath.Separator))
		if inside && (!found || len(wt.Root) > len(best.Root)) {
			best, found = wt, true
		}
	}
	return best, found
}

func discoverRepo(ctx context.Context, repo registry.Repo) ([]Worktree, []Skip, error) {
	root, err := filepath.EvalSymlinks(repo.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, []Skip{{Path: repo.Path, Reason: "checkout missing"}}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", repo.Path, err)
	}
	gitDir, commonDir, err := vcs.GitDirs(root)
	if err != nil {
		return nil, nil, err
	}
	if commonDir == "" {
		return nil, []Skip{{Path: root, Reason: "not a git checkout"}}, nil
	}
	if commonDir, err = filepath.EvalSymlinks(commonDir); err != nil {
		return nil, nil, fmt.Errorf("resolve common dir: %w", err)
	}
	if gitDir, err = filepath.EvalSymlinks(gitDir); err != nil {
		return nil, nil, fmt.Errorf("resolve git dir: %w", err)
	}
	listed, err := listWorktrees(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	base := Worktree{Origin: repo.Origin, Relpath: repo.Relpath, Trunk: repo.Trunk, CommonDir: commonDir}
	var wts []Worktree
	var skips []Skip
	for i, l := range listed {
		if l.bare {
			continue
		}
		if l.prunable {
			skips = append(skips, Skip{Path: l.path, Reason: "prunable"})
			continue
		}
		wtRoot, err := filepath.EvalSymlinks(l.path)
		if errors.Is(err, fs.ErrNotExist) {
			skips = append(skips, Skip{Path: l.path, Reason: "worktree missing"})
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("resolve worktree %s: %w", l.path, err)
		}
		wt := base
		wt.Root, wt.Head, wt.Branch, wt.Locked, wt.Kind = wtRoot, l.head, l.branch, l.locked, KindGit
		admin := commonDir
		if i == 0 && gitDir == commonDir {
			wt.Root = root
		}
		if i > 0 {
			if admin, err = linkedAdminDir(wtRoot, commonDir); err != nil {
				skips = append(skips, Skip{Path: wtRoot, Reason: err.Error()})
				continue
			}
			wt.Name = filepath.Base(admin)
		}
		wt.GitDir = admin
		if i == 0 && isDir(filepath.Join(wt.Root, ".jj")) {
			wt.Kind = KindJJColocated
			admin = filepath.Join(wt.Root, ".jj", "working_copy")
		}
		if wt.Incarnation, err = inode(admin); err != nil {
			return nil, nil, err
		}
		wt.ID = worktreeID(wt.Origin, wt.Root, wt.Incarnation)
		wts = append(wts, wt)
		if wt.Kind == KindJJColocated {
			ws, wsSkips, err := jjWorkspaces(ctx, wt)
			if err != nil {
				return nil, nil, err
			}
			wts = append(wts, ws...)
			skips = append(skips, wsSkips...)
		}
	}
	return wts, skips, nil
}

func listWorktrees(ctx context.Context, root string) ([]listedWorktree, error) {
	var out bytes.Buffer
	err := vcs.Exec(ctx, vcs.Cmd{
		Dir:    root,
		Name:   "git",
		Args:   []string{"-C", root, "worktree", "list", "--porcelain", "-z"},
		Env:    vcs.ReadOnlyGitEnv(),
		Stdout: &out,
	})
	if err != nil {
		return nil, fmt.Errorf("list worktrees of %s: %w", root, err)
	}
	return parseWorktreeList(out.Bytes())
}

func parseWorktreeList(out []byte) ([]listedWorktree, error) {
	var listed []listedWorktree
	var cur *listedWorktree
	for field := range strings.SplitSeq(string(out), "\x00") {
		if field == "" {
			cur = nil
			continue
		}
		key, value, _ := strings.Cut(field, " ")
		if key == "worktree" {
			listed = append(listed, listedWorktree{path: value})
			cur = &listed[len(listed)-1]
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("worktree list field %q outside a record", field)
		}
		switch key {
		case "HEAD":
			cur.head = value
		case "branch":
			cur.branch = strings.TrimPrefix(value, "refs/heads/")
		case "bare":
			cur.bare = true
		case "locked":
			cur.locked = true
		case "prunable":
			cur.prunable = true
		}
	}
	return listed, nil
}

func linkedAdminDir(wtRoot, commonDir string) (string, error) {
	gitDir, _, err := vcs.GitDirs(wtRoot)
	if err != nil {
		return "", err
	}
	if gitDir, err = filepath.EvalSymlinks(gitDir); err != nil {
		return "", fmt.Errorf("resolve admin dir: %w", err)
	}
	if filepath.Dir(gitDir) != filepath.Join(commonDir, "worktrees") {
		return "", fmt.Errorf("admin dir %s outside %s/worktrees", gitDir, commonDir)
	}
	return gitDir, nil
}

func worktreeID(origin, root string, incarnation uint64) string {
	sum := sha256.Sum256([]byte(origin + "\x00" + root + "\x00" + strconv.FormatUint(incarnation, 10)))
	return hex.EncodeToString(sum[:])[:32]
}

func inode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no inode", path)
	}
	return st.Ino, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (w Worktree) check() error {
	switch {
	case !isHex(w.ID, 32):
		return fmt.Errorf("id %q", w.ID)
	case w.Origin == "":
		return fmt.Errorf("empty origin")
	case !filepath.IsAbs(w.Root) || !filepath.IsAbs(w.CommonDir):
		return fmt.Errorf("root %q and common dir %q must be absolute", w.Root, w.CommonDir)
	}
	switch w.Kind {
	case KindGit, KindJJColocated:
		if !filepath.IsAbs(w.GitDir) {
			return fmt.Errorf("git dir %q must be absolute", w.GitDir)
		}
	case KindJJWorkspace:
		if w.GitDir != "" {
			return fmt.Errorf("jj workspace with git dir %q", w.GitDir)
		}
	default:
		return fmt.Errorf("kind %q", w.Kind)
	}
	return nil
}
