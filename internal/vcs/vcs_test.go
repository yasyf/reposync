package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
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
