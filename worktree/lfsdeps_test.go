package worktree_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

const binAttrs = "*.bin filter=lfs diff=lfs merge=lfs -text\n"

type pointerForm func(oid string, size int) string

func canonicalPointer(oid string, size int) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
}

func crlfPointer(oid string, size int) string {
	return strings.ReplaceAll(canonicalPointer(oid, size), "\n", "\r\n")
}

func unterminatedPointer(oid string, size int) string {
	return strings.TrimSuffix(canonicalPointer(oid, size), "\n")
}

func stageRaw(t *testing.T, f *vcstest.Fixture, repo, path string, data []byte) {
	t.Helper()
	blob := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blob, data, 0o600); err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(f.RunGit(repo, "hash-object", "-w", "--no-filters", blob))
	f.RunGit(repo, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+path)
	writeBytes(t, repo, path, data)
}

func wantMissing(t *testing.T, repo string, want []worktree.LFSObjectRef) {
	t.Helper()
	snap, err := capture(t, openStore(t), discoverAt(t, repo, repo), worktreetest.New(), worktree.Limits{})
	var missing *worktree.MissingLFSError
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Objects, want) {
		t.Fatalf("Capture = complete %v lfs %v, error %v; want MissingLFSError %+v", snap.Complete, lfsOIDs(snap), err, want)
	}
}

func TestRequiredLFSDiscovery(t *testing.T) {
	asset := "required asset\x00\x21"
	oid := vcstest.SHA256([]byte(asset))
	tests := []struct {
		name  string
		trunk []string
		form  pointerForm
		path  string
		setup func(t *testing.T, f *vcstest.Fixture, repo, pointer string)
	}{
		{"merge-introduced-then-deleted", []string{"*.bin"}, canonicalPointer, "merge.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			f.RunGit(repo, "checkout", "-qb", "side")
			f.WriteFile(repo, "side.txt", "side\n")
			f.RunGit(repo, "add", "side.txt")
			f.RunGit(repo, "commit", "-qm", "side")
			f.RunGit(repo, "checkout", "-q", "main")
			f.WriteFile(repo, "main.txt", "main\n")
			f.RunGit(repo, "add", "main.txt")
			f.RunGit(repo, "commit", "-qm", "main")
			f.RunGit(repo, "merge", "-q", "--no-ff", "--no-commit", "side")
			stageRaw(t, f, repo, "merge.bin", []byte(pointer))
			f.RunGit(repo, "commit", "-qm", "merge with asset")
			f.RunGit(repo, "rm", "-q", "merge.bin")
			f.RunGit(repo, "commit", "-qm", "remove asset")
		}},
		{"attribute-only-commit", []string{"*.dat"}, canonicalPointer, "asset.bin", func(_ *testing.T, f *vcstest.Fixture, repo, _ string) {
			f.WriteFile(repo, ".gitattributes", f.ReadFile(repo, ".gitattributes")+binAttrs)
			f.RunGit(repo, "commit", "-qam", "track bin")
		}},
		{"attribute-only-stage", []string{"*.dat"}, crlfPointer, "asset.bin", func(_ *testing.T, f *vcstest.Fixture, repo, _ string) {
			f.WriteFile(repo, ".gitattributes", f.ReadFile(repo, ".gitattributes")+binAttrs)
			f.RunGit(repo, "add", ".gitattributes")
		}},
		{"attribute-only-worktree", []string{"*.dat"}, canonicalPointer, "asset.bin", func(_ *testing.T, f *vcstest.Fixture, repo, _ string) {
			f.WriteFile(repo, ".gitattributes", f.ReadFile(repo, ".gitattributes")+binAttrs)
		}},
		{"no-filter-config-commit", []string{"*.bin"}, canonicalPointer, "asset.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			stageRaw(t, f, repo, "asset.bin", []byte(pointer))
			f.RunGit(repo, "commit", "-qm", "asset")
			f.RunGit(repo, "config", "--remove-section", "filter.lfs")
		}},
		{"no-filter-config-stage", []string{"*.bin"}, canonicalPointer, "asset.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			stageRaw(t, f, repo, "asset.bin", []byte(pointer))
			f.RunGit(repo, "config", "--remove-section", "filter.lfs")
		}},
		{"crlf-commit", []string{"*.bin"}, crlfPointer, "asset.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			stageRaw(t, f, repo, "asset.bin", []byte(pointer))
			f.RunGit(repo, "commit", "-qm", "asset")
		}},
		{"crlf-stage", []string{"*.bin"}, crlfPointer, "asset.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			stageRaw(t, f, repo, "asset.bin", []byte(pointer))
		}},
		{"unterminated-commit", []string{"*.bin"}, unterminatedPointer, "asset.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			stageRaw(t, f, repo, "asset.bin", []byte(pointer))
			f.RunGit(repo, "commit", "-qm", "asset")
		}},
		{"unterminated-stage", []string{"*.bin"}, unterminatedPointer, "asset.bin", func(t *testing.T, f *vcstest.Fixture, repo, pointer string) {
			stageRaw(t, f, repo, "asset.bin", []byte(pointer))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			f.EnableLFS(tt.trunk...)
			pointer := tt.form(oid, len(asset))
			if !slices.Contains(tt.trunk, "*.bin") {
				f.RunGit(f.Seed, "config", "lfs.allowincompletepush", "true")
				f.AdvanceOriginPath("asset.bin", pointer)
			}
			repo := f.LFSClone(filepath.Join(f.Root, "repo"))
			tt.setup(t, f, repo, pointer)
			wantMissing(t, repo, []worktree.LFSObjectRef{{Path: tt.path, OID: oid, Size: int64(len(asset))}})
		})
	}
}

func TestMergeIntroducedLFSRoundTrips(t *testing.T) {
	asset := "merge resolution asset\x00\x11"
	oid := vcstest.SHA256([]byte(asset))
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	f.RunGit(rt.src, "checkout", "-qb", "side")
	rt.commit(rt.src, "side.txt", "side\n")
	f.RunGit(rt.src, "checkout", "-q", "main")
	rt.commit(rt.src, "main.txt", "main\n")
	f.RunGit(rt.src, "merge", "-q", "--no-ff", "--no-commit", "side")
	f.WriteFile(rt.src, "merge.bin", asset)
	f.RunGit(rt.src, "add", "merge.bin")
	f.RunGit(rt.src, "commit", "-qm", "merge with asset")
	f.RunGit(rt.src, "rm", "-q", "merge.bin")
	f.RunGit(rt.src, "commit", "-qm", "remove asset")
	snap, _ := rt.tick()
	if !snap.Complete || !slices.Equal(lfsOIDs(snap), []string{oid}) {
		t.Fatalf("complete=%v lfs objects %v, want the merge-introduced object %s", snap.Complete, lfsOIDs(snap), oid)
	}
	for _, dir := range []string{rt.src, filepath.Join(f.Origin, "lfs")} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
	if got, err := os.ReadFile(vcstest.LFSObjectPath(filepath.Join(rt.recv, ".git"), oid)); err != nil || string(got) != asset {
		t.Fatalf("receiver lfs object %q, %v, want %q", got, err, asset)
	}
}

func TestCachedBlobIsReclassified(t *testing.T) {
	asset := "reclassified asset\x00\x1b"
	oid := vcstest.SHA256([]byte(asset))
	f := vcstest.New(t)
	f.EnableLFS("*.dat")
	repo := f.LFSClone(filepath.Join(f.Root, "repo"))
	stageRaw(t, f, repo, "asset.bin", []byte(crlfPointer(oid, len(asset))))
	st, wt, sink := openStore(t), discoverAt(t, repo, repo), worktreetest.New()
	if snap := mustCapture(t, st, wt, sink); len(snap.LFSObjects) != 0 {
		t.Fatalf("lfs objects %v before asset.bin is under filter=lfs", lfsOIDs(snap))
	}
	f.WriteFile(repo, ".gitattributes", f.ReadFile(repo, ".gitattributes")+binAttrs)
	_, err := capture(t, st, wt, sink, worktree.Limits{})
	var missing *worktree.MissingLFSError
	want := []worktree.LFSObjectRef{{Path: "asset.bin", OID: oid, Size: int64(len(asset))}}
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Objects, want) {
		t.Fatalf("recapture with the cached blob: error %v, want MissingLFSError %+v", err, want)
	}
}

func TestCaptureStagedLFSAttributeRemovalKeepsRawBytes(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	tests := []struct {
		name  string
		mtime time.Time
	}{
		{"stat clean", past},
		{"stat changed", time.Now().Add(time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			f.EnableLFS("*.bin")
			f.AdvanceOriginPath("base.bin", lfsBase)
			rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
			asset := filepath.Join(rt.src, "base.bin")
			if err := os.Chtimes(asset, past, past); err != nil {
				t.Fatal(err)
			}
			f.RunGit(rt.src, "update-index", "--refresh")
			f.WriteFile(rt.src, ".gitattributes", datAttrs)
			f.RunGit(rt.src, "add", ".gitattributes")
			if err := os.Chtimes(asset, tt.mtime, tt.mtime); err != nil {
				t.Fatal(err)
			}

			snap, want := rt.tick()
			if got, ok := fileEntry(snap, "base.bin"); !ok || got.Kind != worktree.FileRegular {
				t.Fatalf("base.bin %+v (present %v), want its raw bytes captured", got, ok)
			}
			// git status trusts a stat-clean entry across an attribute change; a
			// fresh mtime makes the source's status re-read base.bin like the receiver's.
			touched := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(asset, touched, touched); err != nil {
				t.Fatal(err)
			}
			want.status = rt.status(rt.src)
			r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
			rt.assertRestored(snap, r, want)
			if got := f.ReadFile(r.Path, "base.bin"); got != lfsBase {
				t.Fatalf("restored base.bin %q, want %q", got, lfsBase)
			}
		})
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

func TestCaptureStatCleanConversionAttributeChange(t *testing.T) {
	for _, staged := range []bool{true, false} {
		t.Run(fmt.Sprintf("staged=%t", staged), func(t *testing.T) {
			f := vcstest.New(t)
			f.AdvanceOriginPath("notes.txt", "one\ntwo\n")
			rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
			past := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(rt.src, "notes.txt"), past, past); err != nil {
				t.Fatal(err)
			}
			f.RunGit(rt.src, "update-index", "--refresh")
			f.WriteFile(rt.src, ".gitattributes", "*.txt text eol=crlf\n")
			if staged {
				f.RunGit(rt.src, "add", ".gitattributes")
			}

			snap, want := rt.tick()
			if _, ok := fileEntry(snap, "notes.txt"); ok != staged {
				t.Fatalf("notes.txt captured %v, want %v", ok, staged)
			}
			r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
			if staged {
				// The checkout records the CRLF file's size in the index, so git
				// status flags the rewritten LF file without rehashing it.
				want.status = rt.status(r.Path)
				if src, got := rt.git(rt.src, "ls-files", "-s"), rt.git(r.Path, "ls-files", "-s"); src != got {
					t.Fatalf("index differs:\nsource   %s\nrestored %s", src, got)
				}
			}
			rt.assertRestored(snap, r, want)
			if got := f.ReadFile(r.Path, "notes.txt"); got != "one\ntwo\n" {
				t.Fatalf("restored notes.txt %q, want the source's LF bytes", got)
			}
		})
	}
}

func TestCaptureAttributeRemovalBesideDuplicatePointer(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	f.AdvanceOriginPath("base.bin", lfsBase)
	f.AdvanceOriginPath("twin.bin", lfsBase)
	rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	asset := filepath.Join(rt.src, "base.bin")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(asset, past, past); err != nil {
		t.Fatal(err)
	}
	f.RunGit(rt.src, "update-index", "--refresh")
	f.WriteFile(rt.src, ".gitattributes", binAttrs+"base.bin !filter !diff !merge !text\n")
	f.RunGit(rt.src, "add", ".gitattributes")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(rt.src, "twin.bin"), future, future); err != nil {
		t.Fatal(err)
	}

	snap, want := rt.tick()
	if got, ok := fileEntry(snap, "base.bin"); !ok || got.Kind != worktree.FileRegular {
		t.Fatalf("base.bin %+v (present %v), want its raw bytes captured", got, ok)
	}
	touched := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(asset, touched, touched); err != nil {
		t.Fatal(err)
	}
	want.status = rt.status(rt.src)
	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
	rt.assertRestored(snap, r, want)
	if got := f.ReadFile(r.Path, "base.bin"); got != lfsBase {
		t.Fatalf("restored base.bin %q, want %q", got, lfsBase)
	}
}

func TestCaptureCommittedNestedAttributeRemovalKeepsRawBytes(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS()
	if err := os.MkdirAll(filepath.Join(f.Seed, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.AdvanceOriginPath("nested/.gitattributes", binAttrs)
	f.AdvanceOriginPath("nested/base.bin", lfsBase)
	rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	asset := filepath.Join(rt.src, "nested", "base.bin")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(asset, past, past); err != nil {
		t.Fatal(err)
	}
	f.RunGit(rt.src, "update-index", "--refresh")
	f.WriteFile(filepath.Join(rt.src, "nested"), ".gitattributes", "*.txt text\n")
	f.RunGit(rt.src, "commit", "-qam", "drop nested LFS")

	snap, want := rt.tick()
	if got, ok := fileEntry(snap, "nested/base.bin"); !ok || got.Kind != worktree.FileRegular {
		t.Fatalf("nested/base.bin %+v (present %v), want its raw bytes captured", got, ok)
	}
	touched := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(asset, touched, touched); err != nil {
		t.Fatal(err)
	}
	want.status = rt.status(rt.src)
	r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")})
	rt.assertRestored(snap, r, want)
	if got := f.ReadFile(r.Path, "nested/base.bin"); got != lfsBase {
		t.Fatalf("restored nested/base.bin %q, want %q", got, lfsBase)
	}
}
