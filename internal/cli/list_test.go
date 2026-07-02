package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestWatchItemBusyOnLockMarker proves watchItem carries the live-operation probe
// onto the wire: a lock marker in the checkout reports Busy with the exact reason,
// and the item reads idle again once the marker is gone.
func TestWatchItemBusyOnLockMarker(t *testing.T) {
	f := vcstest.New(t)
	dl := filepath.Join(f.Root, "data")
	dest := f.GitClone(filepath.Join(dl, "alpha"))
	repo := state.Repo{Relpath: "alpha", Trunk: "main"}

	var errw bytes.Buffer
	item := watchItem(t.Context(), &errw, "alpha", repo, dl)
	if item.Busy || item.BusyReason != "" {
		t.Fatalf("idle item = (busy=%v, reason=%q), want idle", item.Busy, item.BusyReason)
	}

	lock := filepath.Join(dest, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	item = watchItem(t.Context(), &errw, "alpha", repo, dl)
	if !item.Busy {
		t.Fatal("locked item not busy")
	}
	if item.BusyReason != "git index locked" {
		t.Fatalf("busy reason = %q, want git index locked", item.BusyReason)
	}
	// A busy repo still carries its fingerprint and watch dirs — busy defers, never drops.
	if item.Fingerprint == "" || len(item.WatchDirs) == 0 {
		t.Fatalf("busy item lost fingerprint or watch dirs: %+v", item)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	item = watchItem(t.Context(), &errw, "alpha", repo, dl)
	if item.Busy || item.BusyReason != "" {
		t.Fatalf("post-cleanup item = (busy=%v, reason=%q), want idle", item.Busy, item.BusyReason)
	}
	if errw.Len() != 0 {
		t.Fatalf("probe logged unexpectedly: %s", errw.String())
	}
}
