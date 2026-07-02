package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestConsumerCountsBusyOutOfConverged proves svc.sync and svc.reconcile report a
// busy-gated repo in SkippedBusy — not Converged and not an error — and count it
// converged again once the live operation ends.
func TestConsumerCountsBusyOutOfConverged(t *testing.T) {
	f := vcstest.New(t)
	seedRegistry(t, "yasyf@laptop")
	dl := filepath.Join(f.Root, "data")
	dest := f.GitClone(filepath.Join(dl, "alpha"))
	if _, err := state.Update(t.Context(), func(s *state.State) error {
		s.DefaultLocation = dl
		// A fresh clone always looks recently active; drop the recency gate so
		// busy comes from the lock marker alone.
		s.Settings.IdleThreshold = state.Duration(time.Nanosecond)
		s.AddRepo(state.Repo{Relpath: "alpha", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	lock := filepath.Join(dest, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	c := serveConsumer(t)
	sres, err := c.Sync(t.Context(), "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if sres.Converged != 0 || sres.SkippedBusy != 1 {
		t.Fatalf("locked sync = %+v, want Converged 0, SkippedBusy 1", sres)
	}
	rres, err := c.Reconcile(t.Context(), "")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rres.Converged != 0 || rres.SkippedBusy != 1 {
		t.Fatalf("locked reconcile = %+v, want Converged 0, SkippedBusy 1", rres)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	sres, err = c.Sync(t.Context(), "")
	if err != nil {
		t.Fatalf("post-release sync: %v", err)
	}
	if sres.Converged != 1 || sres.SkippedBusy != 0 {
		t.Fatalf("idle sync = %+v, want Converged 1, SkippedBusy 0", sres)
	}
}
