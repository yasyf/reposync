package worktree_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/registry"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCheckGitVersion(t *testing.T) {
	tests := []struct {
		name string
		out  string
		have string
		err  bool
	}{
		{"exact-minimum", "git version 2.44.0\n", "", false},
		{"newer-minor", "git version 2.55.0\n", "", false},
		{"newer-major", "git version 3.0.0\n", "", false},
		{"apple-suffix", "git version 2.50.1 (Apple Git-155)\n", "", false},
		{"windows-suffix", "git version 2.44.0.windows.1\n", "", false},
		{"older-minor", "git version 2.43.5\n", "2.43.5", true},
		{"apple-older", "git version 2.39.5 (Apple Git-154)\n", "2.39.5", true},
		{"older-major", "git version 1.9.9\n", "1.9.9", true},
		{"garbage", "not git\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := worktree.CheckGitVersion(tt.out)
			if (err != nil) != tt.err {
				t.Fatalf("CheckGitVersion(%q) = %v, want error %v", tt.out, err, tt.err)
			}
			var verr *worktree.GitVersionError
			if got := errors.As(err, &verr); got != (tt.have != "") {
				t.Fatalf("CheckGitVersion(%q) = %v, GitVersionError %v", tt.out, err, got)
			}
			if verr != nil && (verr.Have != tt.have || verr.Want != "2.44") {
				t.Fatalf("GitVersionError = %+v, want have %q want 2.44", verr, tt.have)
			}
		})
	}
}

func TestEntryPointsRequireGit244(t *testing.T) {
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	wt := discoverAt(t, repo, repo)
	store := openStore(t)
	bin := t.TempDir()
	//nolint:gosec // G306: the fake git must be executable to shadow the real one on PATH.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.39.5 (Apple Git-154)'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, call := range map[string]func() error{
		"Stamp": func() error {
			_, err := worktree.Stamp(t.Context(), wt)
			return err
		},
		"Capture": func() error {
			_, err := store.Capture(t.Context(), wt, worktreetest.New(), worktree.CaptureOptions{Source: "host"})
			return err
		},
		"Verify": func() error {
			_, err := store.Verify(t.Context(), registry.Registry{}, worktree.Snapshot{}, worktreetest.New(), worktree.VerifyOptions{})
			return err
		},
		"Restore": func() error {
			_, err := store.Restore(t.Context(), registry.Registry{}, worktree.Snapshot{}, worktreetest.New(), worktree.RestoreOptions{Dest: filepath.Join(f.Root, "dest")})
			return err
		},
	} {
		var verr *worktree.GitVersionError
		if err := call(); !errors.As(err, &verr) || verr.Have != "2.39.5" || verr.Want != "2.44" {
			t.Errorf("%s = %v, want *GitVersionError{Have: 2.39.5, Want: 2.44}", name, err)
		}
	}
}
