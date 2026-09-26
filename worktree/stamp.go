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
	"slices"
	"strings"
	"syscall"

	"github.com/yasyf/reposync/internal/vcs"
)

// Stamp returns a hex sha256 over wt's code work in progress: the worktree
// kind, any in-progress git or jj operation marker, HEAD, branch, and upstream,
// the stat of the per-worktree index file, every porcelain-v2 status record,
// and the lstat of every changed and untracked path. An unstaged edit to a
// tracked file or a new non-ignored file changes it with HEAD and the index
// untouched; a change confined to ignored paths does not, because git prunes
// ignored directories itself. Stamp reads the source as Capture does and never
// writes it: no index refresh, no hooks, no filter drivers, and jj reads pinned
// to the current operation without a snapshot. A KindJJWorkspace has no index
// of its own, so its stamp covers the workspace's @ and @- commit ids and the
// lstat of every non-ignored file instead of status records.
func Stamp(ctx context.Context, wt Worktree) (string, error) {
	records, err := stampRecords(ctx, wt)
	if err != nil {
		return "", fmt.Errorf("stamp %s: %w", wt.Root, err)
	}
	sum := sha256.Sum256([]byte(strings.Join(records, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func stampRecords(ctx context.Context, wt Worktree) ([]string, error) {
	jjDir := ""
	if wt.Kind != KindGit {
		jjDir = filepath.Join(wt.Root, ".jj")
	}
	reason, lock, err := vcs.OpState(wt.GitDir, wt.CommonDir, jjDir)
	if err != nil {
		return nil, err
	}
	records := []string{fmt.Sprintf("kind %q", wt.Kind), fmt.Sprintf("op %q %t", reason, lock)}
	if wt.Kind != KindGit {
		ids, err := jjRead(ctx, wt.Root, "log", "--no-graph", "-r", "@ | @-", "-T", `commit_id ++ "\n"`)
		if err != nil {
			return nil, err
		}
		records = append(records, fmt.Sprintf("jj %q", ids))
	}
	var tree, paths []string
	if wt.Kind == KindJJWorkspace {
		paths, err = jjWorkspaceFiles(ctx, wt)
	} else {
		tree, paths, err = gitStatusRecords(ctx, wt)
	}
	if err != nil {
		return nil, err
	}
	stats, err := lstatRecords(wt.Root, paths)
	if err != nil {
		return nil, err
	}
	return slices.Concat(records, tree, stats), nil
}

func gitStatusRecords(ctx context.Context, wt Worktree) ([]string, []string, error) {
	index, err := indexRecord(filepath.Join(wt.GitDir, "index"))
	if err != nil {
		return nil, nil, err
	}
	env, err := vcs.FilterOverrideEnv(ctx, wt.Root)
	if err != nil {
		return nil, nil, err
	}
	st, err := readStatus(ctx, wt.Root, env)
	if err != nil {
		return nil, nil, err
	}
	entries := make([]string, 0, len(st.changed)+len(st.unmerged)+len(st.untracked))
	paths := slices.Concat(st.unmerged, st.untracked)
	for _, e := range st.changed {
		entries = append(entries, fmt.Sprintf("1 %c%c %s %s %s %s %s %s %q",
			e.x, e.y, e.sub, e.modeHead, e.modeIndex, e.modeWorktree, e.oidHead, e.oidIndex, e.path))
		paths = append(paths, e.path)
	}
	for _, p := range st.unmerged {
		entries = append(entries, fmt.Sprintf("u %q", p))
	}
	for _, p := range st.untracked {
		entries = append(entries, fmt.Sprintf("? %q", p))
	}
	slices.Sort(entries)
	head := fmt.Sprintf("head %q %q %q", st.commit, st.branch, st.upstream)
	return slices.Concat([]string{head, index}, entries), paths, nil
}

func indexRecord(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "index absent", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	st, err := statT(info)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	return fmt.Sprintf("index %d %d %d", st.Size, st.Mtimespec.Nano(), st.Ino), nil
}

// A jj workspace has no git index of its own: listing against an absent index
// makes every non-ignored file an untracked "other", git still prunes ignored
// directories, and Capture's private index is never read or written.
func jjWorkspaceFiles(ctx context.Context, wt Worktree) ([]string, error) {
	scratch, err := os.MkdirTemp("", "reposync-stamp-")
	if err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	env := append(vcs.ReadOnlyGitEnv(),
		"GIT_DIR="+wt.CommonDir, "GIT_WORK_TREE="+wt.Root, "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
	var out bytes.Buffer
	err = vcs.Exec(ctx, vcs.Cmd{
		Dir:    wt.Root,
		Name:   "git",
		Args:   []string{"-C", wt.Root, "ls-files", "-z", "--others", "--exclude-standard", "--", ".", ":(exclude,top).jj"},
		Env:    env,
		Stdout: &out,
	})
	if err != nil {
		return nil, fmt.Errorf("list jj workspace files: %w", err)
	}
	return strings.FieldsFunc(out.String(), func(r rune) bool { return r == 0 }), nil
}

func lstatRecords(dir string, paths []string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open root: %w", err)
	}
	defer func() { _ = root.Close() }()
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		clean = append(clean, strings.TrimSuffix(p, "/"))
	}
	slices.Sort(clean)
	clean = slices.Compact(clean)
	records := make([]string, 0, len(clean))
	for _, p := range clean {
		info, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			records = append(records, fmt.Sprintf("lstat %q absent", p))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lstat %s: %w", p, err)
		}
		st, err := statT(info)
		if err != nil {
			return nil, fmt.Errorf("lstat %s: %w", p, err)
		}
		records = append(records, fmt.Sprintf("lstat %q %d %d %d %d %o",
			p, st.Size, st.Mtimespec.Nano(), st.Ctimespec.Nano(), st.Ino, st.Mode))
	}
	return records, nil
}

func statT(info fs.FileInfo) (*syscall.Stat_t, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("no stat_t")
	}
	return st, nil
}
