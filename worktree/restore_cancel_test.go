package worktree_test

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/reposync/worktree"
)

type interruptedSource struct {
	worktree.ArtifactSource
	dest      string
	interrupt func() error
}

func (s interruptedSource) Open(ctx context.Context, ref worktree.ArtifactRef) (io.ReadCloser, error) {
	if _, err := os.Lstat(s.dest); err == nil {
		if err := s.interrupt(); err != nil {
			return nil, err
		}
	}
	return s.ArtifactSource.Open(ctx, ref)
}

type receiverState struct {
	heads     map[string]string
	worktrees string
	status    []string
}

func (h *harness) receiverState() receiverState {
	h.t.Helper()
	return receiverState{
		heads:     h.refs([]string{"-C", h.recv}, "refs/heads/"),
		worktrees: h.git(h.recv, "worktree", "list", "--porcelain"),
		status:    h.status(h.recv),
	}
}

func (h *harness) assertRolledBack(before receiverState, dest string) {
	h.t.Helper()
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("failed restore left %s: %v", dest, err)
	}
	after := h.receiverState()
	if !maps.Equal(before.heads, after.heads) {
		h.t.Fatalf("branches changed:\nbefore %v\nafter  %v", before.heads, after.heads)
	}
	if before.worktrees != after.worktrees {
		h.t.Fatalf("worktrees changed:\nbefore %s\nafter  %s", before.worktrees, after.worktrees)
	}
	if !slices.Equal(before.status, after.status) {
		h.t.Fatalf("receiver status changed:\nbefore %q\nafter  %q", before.status, after.status)
	}
	if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
		h.t.Fatalf("recovery refs left in checkout: %v", refs)
	}
}

func TestRestoreRollsBackOnFailure(t *testing.T) {
	errInjected := errors.New("injected artifact failure")
	tests := []struct {
		name      string
		interrupt func(cancel context.CancelFunc) error
		want      error
	}{
		{"cancelled", func(cancel context.CancelFunc) error { cancel(); return nil }, context.Canceled},
		{"failed", func(context.CancelFunc) error { return errInjected }, errInjected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.RunGit(h.recv, "branch", "recovery/taken")
			h.f.RunGit(h.recv, "worktree", "add", "-q", filepath.Join(h.f.Root, "user-worktree"))
			h.f.WriteFile(h.src, "README.md", "edited\n")
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			before := h.receiverState()
			parent := filepath.Join(h.f.Root, "pickups")
			dest := filepath.Join(parent, "repo", "recovered")
			opts := worktree.RestoreOptions{Dest: dest, Branch: "recovery/taken"}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := interruptedSource{ArtifactSource: h.art, dest: dest, interrupt: func() error { return tt.interrupt(cancel) }}
			if _, err := h.store.Restore(ctx, h.recvReg(), snap, src, opts); !errors.Is(err, tt.want) {
				t.Fatalf("restore error = %v, want %v", err, tt.want)
			}
			h.assertRolledBack(before, dest)
			if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed restore left created parent %s: %v", parent, err)
			}

			r := h.restore(snap, opts)
			if r.Path != dest || r.Reused || r.Branch != "recovery/taken-2" {
				t.Fatalf("retry restored %+v, want a fresh recovery/taken-2 at %s", r, dest)
			}
			h.assertFaithful(r)
		})
	}
}
