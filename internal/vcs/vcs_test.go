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

func TestWatchPaths(t *testing.T) {
	trunkFiles := func(root string, ref ...string) []string {
		return []string{
			filepath.Join(append([]string{root, ".git", "refs", "remotes", "origin"}, ref...)...),
			filepath.Join(append([]string{root, ".git", "logs", "refs", "remotes", "origin"}, ref...)...),
		}
	}
	cases := []struct {
		id    string
		dirs  []string
		trunk string
		want  func(root string) []string
	}{
		{
			id:    "colocated jj adds the op heads dir after the trunk files",
			dirs:  []string{".git", ".jj"},
			trunk: "main",
			want: func(root string) []string {
				return append(trunkFiles(root, "main"), filepath.Join(root, ".jj", "repo", "op_heads", "heads"))
			},
		},
		{
			id:    "plain git watches only the configured trunk's ref and reflog",
			dirs:  []string{".git"},
			trunk: "dev",
			want:  func(root string) []string { return trunkFiles(root, "dev") },
		},
		{
			id:    "a trunk with slashes nests under origin",
			dirs:  []string{".git"},
			trunk: "release/2026",
			want:  func(root string) []string { return trunkFiles(root, "release", "2026") },
		},
		{
			id:    "an empty trunk watches main, the trunk Open resolves",
			dirs:  []string{".git"},
			trunk: "",
			want:  func(root string) []string { return trunkFiles(root, "main") },
		},
		{
			id:    "nonexistent root watches the trunk files of the future clone",
			dirs:  nil,
			trunk: "main",
			want:  func(root string) []string { return trunkFiles(root, "main") },
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
			if got, want := WatchPaths(root, c.trunk), c.want(root); !slices.Equal(got, want) {
				t.Errorf("WatchPaths = %v, want %v", got, want)
			}
		})
	}
}

func TestWatchPathsAreTheTrunkFilesAFetchWrites(t *testing.T) {
	cases := []struct {
		id    string
		clone func(f *vcstest.Fixture, dest string) string
		fetch func(f *vcstest.Fixture, repo string)
	}{
		{
			id:    "plain git",
			clone: (*vcstest.Fixture).GitClone,
			fetch: func(f *vcstest.Fixture, repo string) { f.RunGit(repo, "fetch", "-q", "origin") },
		},
		{
			id:    "colocated jj",
			clone: (*vcstest.Fixture).JJClone,
			fetch: func(f *vcstest.Fixture, repo string) { f.RunJJ(repo, "git", "fetch") },
		},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			f := vcstest.New(t)
			repo := c.clone(f, filepath.Join(f.Root, "repo"))
			paths := WatchPaths(repo, "main")
			ref, reflog := paths[0], paths[1]

			want := f.AdvanceOrigin("two")
			c.fetch(f, repo)
			requireTrunkFiles(t, ref, reflog, want)
			refBefore, reflogBefore := statFile(t, ref), statFile(t, reflog)

			f.RunGit(f.Seed, "commit", "-q", "--allow-empty", "-m", "side")
			f.RunGit(f.Seed, "push", "-q", "origin", "HEAD:refs/heads/side")
			c.fetch(f, repo)
			for _, before := range []struct {
				path string
				info os.FileInfo
			}{{ref, refBefore}, {reflog, reflogBefore}} {
				after := statFile(t, before.path)
				if !os.SameFile(before.info, after) || after.Size() != before.info.Size() || !after.ModTime().Equal(before.info.ModTime()) {
					t.Fatalf("fetching only side touched %s: before (size %d, mtime %v), after (size %d, mtime %v, same file %v)",
						before.path, before.info.Size(), before.info.ModTime(), after.Size(), after.ModTime(), os.SameFile(before.info, after))
				}
			}

			want = f.AdvanceOrigin("three")
			c.fetch(f, repo)
			requireTrunkFiles(t, ref, reflog, want)
			if after := statFile(t, reflog); !os.SameFile(reflogBefore, after) || after.Size() <= reflogBefore.Size() {
				t.Fatalf("trunk move rewrote the reflog (same file %v, size %d -> %d), want an in-place append",
					os.SameFile(reflogBefore, after), reflogBefore.Size(), after.Size())
			}
		})
	}
}

func requireTrunkFiles(t *testing.T, ref, reflog, want string) {
	t.Helper()
	head, err := os.ReadFile(ref) //nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	if err != nil {
		t.Fatalf("read trunk ref: %v", err)
	}
	if got := strings.TrimSpace(string(head)); got != want {
		t.Fatalf("trunk ref %s = %q, want %q", ref, got, want)
	}
	entries, err := os.ReadFile(reflog) //nolint:gosec // G304: test reads a file from a test-controlled temp dir.
	if err != nil {
		t.Fatalf("read trunk reflog: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(entries)), "\n")
	if fields := strings.Fields(lines[len(lines)-1]); len(fields) < 2 || fields[1] != want {
		t.Fatalf("last trunk reflog entry %q, want new value %q", lines[len(lines)-1], want)
	}
}

func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s mode %v, want a regular file", path, info.Mode())
	}
	return info
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
