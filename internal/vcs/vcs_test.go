package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestWatchPaths locks the watch-backend contract (backend-agnostic): the exact
// ordered leaf set per repo shape, including the not-yet-cloned case (watch items
// are built before the clone exists).
func TestWatchPaths(t *testing.T) {
	gitLeaves := func(root string) []string {
		return []string{
			filepath.Join(root, ".git", "refs", "remotes", "origin"),
			filepath.Join(root, ".git", "logs", "refs", "remotes", "origin"),
		}
	}
	cases := []struct {
		id   string
		dirs []string
		want func(root string) []string
	}{
		{
			id:   "colocated jj adds the op heads leaf after the git leaves",
			dirs: []string{".git", ".jj"},
			want: func(root string) []string {
				return append(gitLeaves(root), filepath.Join(root, ".jj", "repo", "op_heads", "heads"))
			},
		},
		{
			id:   "plain git watches only the git leaves",
			dirs: []string{".git"},
			want: gitLeaves,
		},
		{
			id:   "nonexistent root watches the git leaves of the future clone",
			dirs: nil,
			want: gitLeaves,
		},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo")
			for _, d := range c.dirs {
				if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			if got, want := WatchPaths(root), c.want(root); !slices.Equal(got, want) {
				t.Errorf("WatchPaths = %v, want %v", got, want)
			}
		})
	}
}

// trunkOrigin seeds a bare origin whose default branch is branch, and returns a
// plain-git clone of it.
func trunkOrigin(t *testing.T, f *vcstest.Fixture, branch string) string {
	t.Helper()
	origin := filepath.Join(f.Root, branch+".git")
	seed := filepath.Join(f.Root, branch+"-seed")
	f.RunGit(f.Root, "init", "--bare", "-b", branch, origin)
	f.RunGit(f.Root, "clone", origin, seed)
	f.ConfigGit(seed)
	f.WriteFile(seed, "README.md", "hello\n")
	f.RunGit(seed, "add", "README.md")
	f.RunGit(seed, "commit", "-qm", "init")
	f.RunGit(seed, "push", "-q", "origin", branch)
	dest := filepath.Join(f.Root, branch+"-clone")
	f.RunGit(f.Root, "clone", origin, dest)
	return dest
}

func TestDetectTrunk(t *testing.T) {
	f := vcstest.New(t)
	dest := trunkOrigin(t, f, "release")

	t.Run("recorded origin/HEAD", func(t *testing.T) {
		if got := DetectTrunk(context.Background(), dest); got != "release" {
			t.Fatalf("DetectTrunk = %q, want release", got)
		}
	})

	t.Run("origin symref when origin/HEAD is unset", func(t *testing.T) {
		f.RunGit(dest, "remote", "set-head", "origin", "--delete")
		if got := DetectTrunk(context.Background(), dest); got != "release" {
			t.Fatalf("DetectTrunk = %q, want release", got)
		}
	})

	t.Run("no origin falls back to main", func(t *testing.T) {
		solo := filepath.Join(f.Root, "solo")
		f.RunGit(f.Root, "init", "-b", "wip", solo)
		if got := DetectTrunk(context.Background(), solo); got != defaultTrunk {
			t.Fatalf("DetectTrunk = %q, want %q", got, defaultTrunk)
		}
	})
}

// TestDetectTrunkAmbiguousBranchName proves a local branch literally named
// origin/<trunk> does not poison detection: `symbolic-ref --short` disambiguates
// that to remotes/origin/<trunk>, which would be stored as a trunk that resolves
// to nothing and stops the repo syncing.
func TestDetectTrunkAmbiguousBranchName(t *testing.T) {
	f := vcstest.New(t)
	dest := trunkOrigin(t, f, "release")
	f.RunGit(dest, "branch", "origin/release")

	short := strings.TrimSpace(f.RunGit(dest, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"))
	if short != "remotes/origin/release" {
		t.Fatalf("precondition: --short = %q, want the ambiguous remotes/origin/release", short)
	}
	if got := DetectTrunk(context.Background(), dest); got != "release" {
		t.Fatalf("DetectTrunk = %q, want release", got)
	}
}

func TestPushURLs(t *testing.T) {
	f := vcstest.New(t)

	t.Run("fetch url when no pushurl is set", func(t *testing.T) {
		dest := f.GitClone(filepath.Join(f.Root, "plain"))
		got, err := PushURLs(context.Background(), dest)
		if err != nil {
			t.Fatalf("push urls: %v", err)
		}
		if !slices.Equal(got, []string{f.Origin}) {
			t.Fatalf("PushURLs = %v, want [%q]", got, f.Origin)
		}
	})

	t.Run("pushurls replace the fetch url", func(t *testing.T) {
		dest := f.GitClone(filepath.Join(f.Root, "pushurl"))
		first := filepath.Join(f.Root, "first.git")
		second := filepath.Join(f.Root, "second.git")
		f.RunGit(dest, "remote", "set-url", "--push", "origin", first)
		f.RunGit(dest, "remote", "set-url", "--push", "--add", "origin", second)

		got, err := PushURLs(context.Background(), dest)
		if err != nil {
			t.Fatalf("push urls: %v", err)
		}
		if !slices.Equal(got, []string{first, second}) {
			t.Fatalf("PushURLs = %v, want both pushurls %v", got, []string{first, second})
		}
	})

	t.Run("no origin remote errors", func(t *testing.T) {
		dest := f.GitClone(filepath.Join(f.Root, "noremote"))
		f.RunGit(dest, "remote", "remove", "origin")
		if _, err := PushURLs(context.Background(), dest); err == nil {
			t.Fatal("PushURLs err = nil with no origin remote, want the failure surfaced")
		}
	})
}

func TestIsWorkingCopyContention(t *testing.T) {
	cases := []struct {
		id   string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"concurrent checkout", fmt.Errorf("jj new main: %w", errors.New("Internal error: Failed to check out commit 99366219: Concurrent checkout")), true},
		{"concurrent working copy operation", errors.New("Concurrent working copy operation"), true},
		{"failed checkout", errors.New("Internal error: Failed to check out commit deadbeef"), true},
		{"unrelated", errors.New("jj git fetch: exit status 1: network unreachable"), false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := IsWorkingCopyContention(c.err); got != c.want {
				t.Errorf("IsWorkingCopyContention(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
