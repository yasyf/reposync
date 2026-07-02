package vcs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

func TestCloneColocated(t *testing.T) {
	f := vcstest.New(t)
	dest := filepath.Join(f.Root, "fresh")
	if err := Clone(context.Background(), f.Origin, dest); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if !f.FileExists(dest, ".jj") {
		t.Fatal(".jj missing: clone was not colocated jj")
	}
	if !f.FileExists(dest, ".git") {
		t.Fatal(".git missing: clone lacks git backing")
	}
	if origin := strings.TrimSpace(f.RunGit(dest, "-C", dest, "remote", "get-url", "origin")); origin != f.Origin {
		t.Fatalf("origin = %q, want %q", origin, f.Origin)
	}
	bookmarks := f.RunJJ(dest, "bookmark", "list", "--all", "--ignore-working-copy",
		"-T", `if(remote && remote != "git", name ++ "@" ++ remote ++ " tracked=" ++ tracked ++ "\n", "")`)
	if !strings.Contains(bookmarks, "main@origin tracked=true") {
		t.Fatalf("bookmarks = %q, want main@origin tracked=true", bookmarks)
	}
}
