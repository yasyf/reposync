package worktree_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCaptureStagedLFSAttributeRemovalKeepsRawBytes(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	f.AdvanceOriginPath("base.bin", lfsBase)
	rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	f.WriteFile(rt.src, ".gitattributes", datAttrs)
	f.RunGit(rt.src, "add", ".gitattributes")

	snap, want := rt.tick()
	if got, ok := fileEntry(snap, "base.bin"); !ok || got.Kind != worktree.FileRegular {
		t.Fatalf("base.bin %+v (present %v), want its raw bytes captured", got, ok)
	}
	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
	rt.assertRestored(snap, r, want)
	if got := f.ReadFile(r.Path, "base.bin"); got != lfsBase {
		t.Fatalf("restored base.bin %q, want %q", got, lfsBase)
	}
}

func TestCaptureHiddenAttributeEditRequiresLFSObject(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.dat")
	asset := "hidden attr dependency\x00\x03"
	oid := vcstest.SHA256([]byte(asset))
	f.RunGit(f.Seed, "config", "lfs.allowincompletepush", "true")
	f.AdvanceOriginPath("asset.bin", crlfPointer(oid, len(asset)))
	repo := f.LFSClone(filepath.Join(f.Root, "repo"))
	f.RunGit(repo, "update-index", "--assume-unchanged", ".gitattributes")
	f.WriteFile(repo, ".gitattributes", datAttrs+binAttrs)

	_, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
	var missing *worktree.MissingLFSError
	if !errors.As(err, &missing) || len(missing.Objects) != 1 || missing.Objects[0].OID != oid {
		t.Fatalf("err %v, want MissingLFSError for %s", err, oid)
	}
}

func TestCapturePublishedLFSRenameShipsNothing(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "staged", true: "committed"}[commit], func(t *testing.T) {
			f := vcstest.New(t)
			f.EnableLFS("*.bin")
			f.AdvanceOriginPath("base.bin", lfsBase)
			repo := f.LFSClone(filepath.Join(f.Root, "repo"))
			f.RunGit(repo, "mv", "base.bin", "renamed.bin")
			if commit {
				f.RunGit(repo, "commit", "-qm", "rename published base asset")
			}
			if err := os.Remove(vcstest.LFSObjectPath(filepath.Join(repo, ".git"), vcstest.SHA256([]byte(lfsBase)))); err != nil {
				t.Fatal(err)
			}

			snap, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
			if err != nil || !snap.Complete || len(snap.LFSObjects) != 0 {
				t.Fatalf("err %v complete=%v lfs objects %v, want the published object left to the receiver", err, snap.Complete, lfsOIDs(snap))
			}
		})
	}
}
