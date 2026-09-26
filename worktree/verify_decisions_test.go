package worktree_test

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/reposync/worktree"
)

func TestVerifyCorruptCachedStagedBlob(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "staged.txt", "original staged bytes\n")
	h.f.RunGit(h.src, "add", "staged.txt")
	snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
	if v := h.verify(snap, worktree.VerifyOptions{}); !v.Ready {
		t.Fatalf("first verify: %+v", v)
	}
	oid := h.git(h.src, "rev-parse", ":staged.txt")
	path := filepath.Join(h.mirrorDir(), "objects", oid[:2], oid[2:])
	var encoded bytes.Buffer
	zw := zlib.NewWriter(&encoded)
	wrong := "CORRUPTED staged bytes\n"
	if _, err := fmt.Fprintf(zw, "blob %d\x00%s", len(wrong), wrong); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := h.store.Verify(t.Context(), h.recvReg(), snap, h.art, worktree.VerifyOptions{})
	if err != nil || !v.Ready {
		return
	}
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")}))
}

func TestVerifyPinsBundlePrerequisites(t *testing.T) {
	h := newGitHarness(t)
	h.commit(h.src, "feature.txt", "private history\n")
	snap := mustCapture(t, openStore(t), discoverAt(t, h.src, h.src), h.art)
	prereq := snap.History[0].Prerequisites[0]
	snap.Requires = nil
	snap = h.seal(snap)
	v, err := h.store.Verify(t.Context(), h.recvReg(), snap, h.art, worktree.VerifyOptions{})
	if err != nil || !v.Ready {
		return
	}
	if pins := h.pins(); !slices.Contains(pins, prereq) {
		root := h.git(h.recv, "commit-tree", "HEAD^{tree}", "-m", "new root")
		h.git(h.recv, "update-ref", "refs/heads/main", root)
		h.git(h.recv, "update-ref", "refs/remotes/origin/main", root)
		h.git(h.recv, "reflog", "expire", "--expire=now", "--all")
		h.git(h.recv, "gc", "-q", "--prune=now")
		_, rerr := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "dest")})
		t.Fatalf("verify ready with pins %v lacking bundle prerequisite %s; after receiver gc restore err = %v", pins, prereq, rerr)
	}
}
