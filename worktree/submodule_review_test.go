package worktree_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCaptureStagedGitlinkIsOmitted(t *testing.T) {
	f := vcstest.New(t)
	f.RunGit(f.Seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", f.Origin, "sub")
	f.RunGit(f.Seed, "commit", "-qm", "add sub")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	src := f.GitClone(filepath.Join(f.Root, "src"))
	f.RunGit(src, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	sub := filepath.Join(src, "sub")
	f.ConfigGit(sub)
	f.WriteFile(sub, "UNPUBLISHED", "only copy of work\n")
	f.RunGit(sub, "add", "UNPUBLISHED")
	f.RunGit(sub, "commit", "-qm", "private submodule work")
	f.RunGit(src, "add", "sub")

	snap := mustCapture(t, openStore(t), discoverAt(t, src, src), worktreetest.New())
	want := []worktree.Omission{{Path: "sub", Reason: worktree.OmitSubmodule}}
	if snap.Complete || !slices.Equal(snap.Omitted, want) {
		t.Fatalf("complete=%v omitted %+v, want %+v", snap.Complete, snap.Omitted, want)
	}
}
