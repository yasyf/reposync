package worktree_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCaptureTransientAttributeCommitsRequireLFSObject(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.dat")
	asset := "historic dependency\x00\x02"
	oid := vcstest.SHA256([]byte(asset))
	f.RunGit(f.Seed, "config", "lfs.allowincompletepush", "true")
	f.AdvanceOriginPath("asset.bin", crlfPointer(oid, len(asset)))
	repo := f.LFSClone(filepath.Join(f.Root, "repo"))
	f.WriteFile(repo, ".gitattributes", datAttrs+binAttrs)
	f.RunGit(repo, "add", ".gitattributes")
	f.RunGit(repo, "commit", "-qm", "enable binary LFS")
	f.WriteFile(repo, ".gitattributes", datAttrs)
	f.RunGit(repo, "add", ".gitattributes")
	f.RunGit(repo, "commit", "-qm", "disable binary LFS")

	_, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
	var missing *worktree.MissingLFSError
	if !errors.As(err, &missing) || len(missing.Objects) != 1 || missing.Objects[0].OID != oid {
		t.Fatalf("err %v, want MissingLFSError for %s", err, oid)
	}
}

func TestCaptureSplitIndexSourceContentUntouched(t *testing.T) {
	f := vcstest.New(t)
	f.RunGit(f.Seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", f.Origin, "sub")
	f.RunGit(f.Seed, "commit", "-qm", "add sub")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	f.RunGit(repo, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	f.WriteFile(repo, "README.md", "unstaged\n")
	f.RunGit(repo, "update-index", "--split-index")
	f.RunGit(filepath.Join(repo, "sub"), "update-index", "--split-index")
	gitDir := filepath.Join(repo, ".git")
	for _, dir := range []string{gitDir, filepath.Join(gitDir, "modules", "sub")} {
		if shared, _ := filepath.Glob(filepath.Join(dir, "sharedindex.*")); len(shared) != 1 {
			t.Fatalf("%s: shared indexes %v, want exactly one", dir, shared)
		}
	}
	before := contentState(t, gitDir)

	wt := discoverAt(t, repo, repo)
	snap := mustCapture(t, openStore(t), wt, worktreetest.New())
	if _, err := worktree.Stamp(t.Context(), wt); err != nil {
		t.Fatal(err)
	}
	if !snap.Complete {
		t.Fatalf("omitted %+v", snap.Omitted)
	}
	if after := contentState(t, gitDir); !maps.Equal(before, after) {
		t.Fatalf("source .git content changed:\nbefore %v\nafter  %v", before, after)
	}
}

func contentState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		//nolint:gosec // G304: hashing files under a test-controlled source .git.
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		state[p] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
