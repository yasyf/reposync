package worktree_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

const datAttrs = "*.dat filter=lfs diff=lfs merge=lfs -text\n"

func TestHistoryLFSSurvivesAttributeEdits(t *testing.T) {
	assets := []string{"unpublished asset\x00\x06", "staged asset\x00\x09"}
	oids := []string{vcstest.SHA256([]byte(assets[0])), vcstest.SHA256([]byte(assets[1]))}
	slices.Sort(oids)
	tests := []struct {
		name string
		edit func(rt *roundTrip)
	}{
		{"worktree-attributes-edit", func(rt *roundTrip) {
			rt.f.WriteFile(rt.src, ".gitattributes", datAttrs)
		}},
		{"staged-attributes-edit", func(rt *roundTrip) {
			rt.f.WriteFile(rt.src, ".gitattributes", datAttrs)
			rt.f.RunGit(rt.src, "add", ".gitattributes")
		}},
		{"committed-attributes-edit", func(rt *roundTrip) {
			rt.commit(rt.src, ".gitattributes", datAttrs)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			f.EnableLFS("*.bin")
			f.AdvanceOriginPath("base.bin", lfsBase)
			rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
			rt.commit(rt.src, "unpublished.bin", assets[0])
			rt.f.WriteFile(rt.src, "staged.bin", assets[1])
			rt.f.RunGit(rt.src, "add", "staged.bin")
			tt.edit(rt)
			snap, _ := rt.tick()
			if !snap.Complete || !slices.Equal(lfsOIDs(snap), oids) {
				t.Fatalf("complete=%v lfs objects %v, want the unpublished and staged objects %v", snap.Complete, lfsOIDs(snap), oids)
			}
			for _, dir := range []string{rt.src, filepath.Join(f.Origin, "lfs")} {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			}
			rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
			for _, asset := range assets {
				got, err := os.ReadFile(vcstest.LFSObjectPath(filepath.Join(rt.recv, ".git"), vcstest.SHA256([]byte(asset))))
				if err != nil || string(got) != asset {
					t.Fatalf("receiver lfs object %q, %v, want %q", got, err, asset)
				}
			}
		})
	}
}

func TestHistoryLFSMissingObjectIsNeverComplete(t *testing.T) {
	asset := "vanished asset\x00\x07"
	oid := vcstest.SHA256([]byte(asset))
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	repo := f.LFSClone(filepath.Join(f.Root, "repo"))
	f.WriteFile(repo, "vanished.bin", asset)
	f.RunGit(repo, "add", "vanished.bin")
	f.RunGit(repo, "commit", "-qm", "vanished")
	f.WriteFile(repo, ".gitattributes", datAttrs)
	if err := os.Remove(vcstest.LFSObjectPath(filepath.Join(repo, ".git"), oid)); err != nil {
		t.Fatal(err)
	}
	_, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
	var missing *worktree.MissingLFSError
	want := []worktree.LFSObjectRef{{Path: "vanished.bin", OID: oid, Size: int64(len(asset))}}
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Objects, want) {
		t.Fatalf("Capture error %v, want MissingLFSError %+v", err, want)
	}
}

func TestStrictPointerOutsideAttributesIsRequired(t *testing.T) {
	asset := "pointer only asset\x00\x08"
	oid := vcstest.SHA256([]byte(asset))
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	repo := f.LFSClone(filepath.Join(f.Root, "repo"))
	f.WriteFile(repo, "hidden.txt", fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(asset)))
	f.RunGit(repo, "add", "hidden.txt")
	f.RunGit(repo, "commit", "-qm", "pointer outside attributes")
	_, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
	var missing *worktree.MissingLFSError
	want := []worktree.LFSObjectRef{{Path: "hidden.txt", OID: oid, Size: int64(len(asset))}}
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Objects, want) {
		t.Fatalf("Capture error %v, want MissingLFSError %+v", err, want)
	}
}

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

func TestCaptureLiteralAttributeDirectoryRequiresLFSObject(t *testing.T) {
	for _, dir := range []string{":(top)nested", ":(literal)nested", "nested*"} {
		t.Run(dir, func(t *testing.T) {
			f := vcstest.New(t)
			f.EnableLFS("*.dat")
			asset := "literal directory dependency\x00\x04"
			oid := vcstest.SHA256([]byte(asset))
			f.RunGit(f.Seed, "config", "lfs.allowincompletepush", "true")
			if err := os.MkdirAll(filepath.Join(f.Seed, dir), 0o750); err != nil {
				t.Fatal(err)
			}
			f.WriteFile(filepath.Join(f.Seed, dir), "asset.bin", crlfPointer(oid, len(asset)))
			f.RunGit(f.Seed, "add", "-A")
			f.RunGit(f.Seed, "commit", "-qm", "publish pointer text")
			f.RunGit(f.Seed, "push", "-q", "origin", "main")
			repo := f.LFSClone(filepath.Join(f.Root, "repo"))
			f.WriteFile(filepath.Join(repo, dir), ".gitattributes", binAttrs)
			f.RunGit(repo, "add", "-A")
			f.RunGit(repo, "commit", "-qm", "enable nested binary LFS")
			f.RunGit(repo, "--literal-pathspecs", "rm", "-q", dir+"/.gitattributes")
			f.RunGit(repo, "commit", "-qm", "disable nested binary LFS")

			_, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
			var missing *worktree.MissingLFSError
			if !errors.As(err, &missing) || len(missing.Objects) != 1 || missing.Objects[0].OID != oid {
				t.Fatalf("err %v, want MissingLFSError for %s", err, oid)
			}
		})
	}
}
