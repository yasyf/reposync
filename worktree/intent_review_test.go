package worktree_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCaptureIntentToAddOverHeadPath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *vcstest.Fixture, src string)
		file  worktree.FileEntry
	}{
		{"deleted", func(_ *vcstest.Fixture, src string) {
			if err := os.Remove(filepath.Join(src, "README.md")); err != nil {
				t.Fatal(err)
			}
		}, worktree.FileEntry{Path: "README.md", Kind: worktree.FileDeleted}},
		{"assume-unchanged", func(f *vcstest.Fixture, src string) {
			f.RunGit(src, "update-index", "--assume-unchanged", "README.md")
			f.WriteFile(src, "README.md", "hidden intent-to-add\n")
		}, worktree.FileEntry{Path: "README.md", Kind: worktree.FileRegular, AssumeUnchanged: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
			f.RunGit(rt.src, "rm", "-q", "--cached", "README.md")
			f.RunGit(rt.src, "add", "-N", "README.md")
			tt.setup(f, rt.src)

			snap, _ := rt.tick()
			if !snap.Complete || len(snap.IntentToAdd) != 1 || snap.IntentToAdd[0].Path != "README.md" {
				t.Fatalf("complete=%v intent-to-add %+v, want README.md", snap.Complete, snap.IntentToAdd)
			}
			if !slices.Equal(snap.Index, []worktree.IndexEntry{{Path: "README.md"}}) {
				t.Fatalf("index %+v, want only the README.md removal", snap.Index)
			}
			got, ok := fileEntry(snap, "README.md")
			if !ok || got.Kind != tt.file.Kind || got.AssumeUnchanged != tt.file.AssumeUnchanged {
				t.Fatalf("file %+v (present %v), want %+v", got, ok, tt.file)
			}
		})
	}
}

func TestCaptureIntentToAddIndexMode(t *testing.T) {
	tests := []struct {
		name    string
		symlink bool
		after   func(t *testing.T, f *vcstest.Fixture, src string)
		want    string
	}{
		{"deleted executable", false, func(t *testing.T, _ *vcstest.Fixture, src string) {
			if err := os.Remove(filepath.Join(src, "ita")); err != nil {
				t.Fatal(err)
			}
		}, "100755"},
		{"deleted symlink", true, func(t *testing.T, _ *vcstest.Fixture, src string) {
			if err := os.Remove(filepath.Join(src, "ita")); err != nil {
				t.Fatal(err)
			}
		}, "120000"},
		{"chmod after add", false, func(t *testing.T, _ *vcstest.Fixture, src string) {
			if err := os.Chmod(filepath.Join(src, "ita"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "100755"},
		{"symlink replaced by file", true, func(t *testing.T, f *vcstest.Fixture, src string) {
			if err := os.Remove(filepath.Join(src, "ita")); err != nil {
				t.Fatal(err)
			}
			f.WriteFile(src, "ita", "regular now\n")
		}, "120000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			src := f.GitClone(filepath.Join(f.Root, "src"))
			if tt.symlink {
				if err := os.Symlink("README.md", filepath.Join(src, "ita")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeBytes(t, src, "ita", []byte("new work\n"))
				//nolint:gosec // G302: the intent-to-add entry must record an executable index mode.
				if err := os.Chmod(filepath.Join(src, "ita"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			f.RunGit(src, "add", "-N", "ita")
			tt.after(t, f, src)

			snap := mustCapture(t, openStore(t), discoverAt(t, src, src), worktreetest.New())
			want := []worktree.IntentToAdd{{Path: "ita", Mode: tt.want}}
			if !snap.Complete || !slices.Equal(snap.IntentToAdd, want) {
				t.Fatalf("complete=%v intent-to-add %+v, want %+v", snap.Complete, snap.IntentToAdd, want)
			}
		})
	}
}

func TestCaptureSparseDeletedIntentToAdd(t *testing.T) {
	f := vcstest.New(t)
	writeBytes(t, f.Seed, "kept/base", []byte("base\n"))
	f.RunGit(f.Seed, "add", "-A")
	f.RunGit(f.Seed, "commit", "-qm", "base")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	src := f.GitClone(filepath.Join(f.Root, "src"))
	f.RunGit(src, "sparse-checkout", "set", "kept")
	f.WriteFile(src, "ita", "new\n")
	f.RunGit(src, "add", "-N", "ita")
	f.RunGit(src, "update-index", "--skip-worktree", "ita")
	if err := os.Remove(filepath.Join(src, "ita")); err != nil {
		t.Fatal(err)
	}

	snap := mustCapture(t, openStore(t), discoverAt(t, src, src), worktreetest.New())
	if !snap.Complete || len(snap.IntentToAdd) != 1 || snap.IntentToAdd[0].Path != "ita" {
		t.Fatalf("complete=%v intent-to-add %+v, want ita", snap.Complete, snap.IntentToAdd)
	}
	got, ok := fileEntry(snap, "ita")
	if want := (worktree.FileEntry{Path: "ita", Kind: worktree.FileDeleted, SkipWorktree: true}); !ok || got != want {
		t.Fatalf("file %+v (present %v), want %+v", got, ok, want)
	}
}
