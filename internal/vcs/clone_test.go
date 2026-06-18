package vcs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneColocated(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(f.root, "fresh")
	if err := Clone(context.Background(), f.origin, dest); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if !f.fileExists(dest, ".jj") {
		t.Fatal(".jj missing: clone was not colocated jj")
	}
	if !f.fileExists(dest, ".git") {
		t.Fatal(".git missing: clone lacks git backing")
	}
	if origin := strings.TrimSpace(f.runGit(dest, "-C", dest, "remote", "get-url", "origin")); origin != f.origin {
		t.Fatalf("origin = %q, want %q", origin, f.origin)
	}
	bookmarks := f.runJJ(dest, "bookmark", "list", "--all", "--ignore-working-copy",
		"-T", `if(remote && remote != "git", name ++ "@" ++ remote ++ " tracked=" ++ tracked ++ "\n", "")`)
	if !strings.Contains(bookmarks, "main@origin tracked=true") {
		t.Fatalf("bookmarks = %q, want main@origin tracked=true", bookmarks)
	}
}
