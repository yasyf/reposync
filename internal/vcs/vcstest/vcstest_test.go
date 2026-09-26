package vcstest

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnableLFSRoundTripsOffline(t *testing.T) {
	f := New(t)
	f.EnableLFS("*.bin")
	data := make([]byte, 4096)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	f.WriteFile(f.Seed, "asset.bin", string(data))
	f.RunGit(f.Seed, "add", "asset.bin")
	f.RunGit(f.Seed, "commit", "-qm", "asset")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")

	oid := SHA256(data)
	if _, err := os.Stat(LFSObjectPath(f.Origin, oid)); err != nil {
		t.Fatalf("origin lfs object: %v", err)
	}
	pointer := f.RunGit(f.Seed, "cat-file", "-p", "HEAD:asset.bin")
	if !strings.Contains(pointer, "oid sha256:"+oid) {
		t.Fatalf("HEAD:asset.bin = %q, want an lfs pointer to %s", pointer, oid)
	}

	clone := f.LFSClone(filepath.Join(f.Root, "clone"))
	if got := f.ReadFile(clone, "asset.bin"); got != string(data) {
		t.Fatalf("cloned asset.bin is %d bytes, want the %d hydrated bytes", len(got), len(data))
	}
	if st := f.RunGit(clone, "status", "--porcelain"); st != "" {
		t.Fatalf("clone status = %q, want clean", st)
	}
}

func TestSnapshotTreeDetectsChanges(t *testing.T) {
	f := New(t)
	dir := filepath.Join(f.Root, "tree")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.WriteFile(dir, "sub/a.txt", "a")
	if err := os.Symlink("sub/a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	before := f.SnapshotTree(dir)
	if got := before["link"].Content; got != "sub/a.txt" {
		t.Fatalf("link content = %q, want sub/a.txt", got)
	}
	if got := before["sub/a.txt"].Content; got != SHA256([]byte("a")) {
		t.Fatalf("sub/a.txt content = %q, want sha256 of a", got)
	}

	tests := []struct {
		name   string
		mutate func()
	}{
		{"content", func() { f.WriteFile(dir, "sub/a.txt", "b") }},
		{"mode", func() {
			if err := os.Chmod(filepath.Join(dir, "sub", "a.txt"), 0o400); err != nil {
				t.Fatalf("chmod: %v", err)
			}
		}},
		{"new file", func() { f.WriteFile(dir, "new", "") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := f.SnapshotTree(dir)
			tc.mutate()
			after := f.SnapshotTree(dir)
			if len(after) == len(snap) && after["sub/a.txt"] == snap["sub/a.txt"] {
				t.Fatalf("snapshot unchanged after %s mutation", tc.name)
			}
		})
	}
}
