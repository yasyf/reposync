package worktree_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/worktree"
)

func (h *harness) worktreeOn(branch string) string {
	h.t.Helper()
	var path string
	for l := range strings.Lines(h.git(h.recv, "worktree", "list", "--porcelain")) {
		if p, ok := strings.CutPrefix(strings.TrimSuffix(l, "\n"), "worktree "); ok {
			path = p
		}
		if strings.TrimSuffix(l, "\n") == "branch refs/heads/"+branch {
			return path
		}
	}
	h.t.Fatalf("no worktree on %s", branch)
	return ""
}

func (h *harness) staged(dest string) string {
	h.t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dest), ".reposync-restore-*", filepath.Base(dest)))
	if err != nil || len(matches) != 1 {
		h.t.Fatalf("staged checkouts for %s = %v, %v; want exactly one", dest, matches, err)
	}
	return matches[0]
}

func (h *harness) assertNoStaging(dir string) {
	h.t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".reposync-") {
			h.t.Fatalf("restore left %s in %s", e.Name(), dir)
		}
	}
}

func TestRestorePublishesLinkedWorktree(t *testing.T) {
	for _, relative := range []string{"false", "true"} {
		t.Run("relative paths "+relative, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.RunGit(h.recv, "config", "worktree.useRelativePaths", relative)
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			dest := filepath.Join(h.f.Root, "pickups", "recovered")
			r := h.restore(snap, worktree.RestoreOptions{Dest: dest})
			h.assertFaithful(r)
			if got := h.worktreeOn(r.Branch); got != dest {
				t.Fatalf("%s is checked out at %s, want %s", r.Branch, got, dest)
			}
			if list := h.git(h.recv, "worktree", "list", "--porcelain"); strings.Contains(list, "prunable") {
				t.Fatalf("published worktree is prunable:\n%s", list)
			}
			if got := h.git(dest, "rev-parse", "--show-toplevel"); got != dest {
				t.Fatalf("published worktree resolves its top level to %s, want %s", got, dest)
			}
			h.assertNoStaging(filepath.Dir(dest))
			if again := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "unused")}); !again.Reused || again.Path != dest {
				t.Fatalf("second restore = %+v, want the published worktree reused", again)
			}
		})
	}
}

func TestRestoreKeepsForeignFilesAtDest(t *testing.T) {
	tests := []struct {
		name   string
		cancel bool
		want   error
	}{
		{"cancelled", true, context.Canceled},
		{"completed", false, worktree.ErrDestinationExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, snap, lfs := lfsFetchHarness(t)
			before := h.receiverState()
			dest := filepath.Join(h.f.Root, "recovered")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gate := func(ctx context.Context, fetch func(context.Context) error) error {
				fetchCtx, stop := context.WithCancel(ctx)
				defer stop()
				go func() {
					select {
					case <-lfs.started:
						if err := os.WriteFile(filepath.Join(dest, "mine.txt"), []byte("user work\n"), 0o600); err != nil {
							t.Errorf("write user file at dest: %v", err)
						}
						if tt.cancel {
							cancel()
						} else {
							stop()
						}
					case <-fetchCtx.Done():
					}
				}()
				return fmt.Errorf("%w: %w", worktree.ErrFetchDeferred, fetch(fetchCtx))
			}
			r, err := h.store.Restore(ctx, h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: dest, FetchLFS: gate})
			if !errors.Is(err, tt.want) {
				t.Fatalf("restore racing a user file at dest = %+v, %v; want %v", r, err, tt.want)
			}
			entries, err := os.ReadDir(dest)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "mine.txt" || h.f.ReadFile(dest, "mine.txt") != "user work\n" {
				t.Fatalf("dest holds %v after rollback, want only the user's mine.txt", entries)
			}
			after := h.receiverState()
			if !maps.Equal(before.heads, after.heads) || before.worktrees != after.worktrees || !slices.Equal(before.status, after.status) {
				t.Fatalf("receiver changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
				t.Fatalf("recovery refs left in checkout: %v", refs)
			}
			h.assertNoStaging(h.f.Root)
		})
	}
}

func TestRestoreRollbackSparesTakenOverBranch(t *testing.T) {
	const branch = "recovery/raced"
	tests := []struct {
		name  string
		race  func(h *harness, snap worktree.Snapshot, stage string)
		other string
	}{
		{"symbolic ref swap", func(h *harness, _ worktree.Snapshot, _ string) {
			h.f.RunGit(h.recv, "symbolic-ref", "refs/heads/"+branch, "refs/heads/main")
		}, ""},
		{"deleted and recreated", func(h *harness, snap worktree.Snapshot, _ string) {
			h.f.RunGit(h.recv, "update-ref", "-d", "refs/heads/"+branch)
			h.f.RunGit(h.recv, "branch", branch, snap.Head.Commit)
		}, ""},
		{"checked out elsewhere", func(h *harness, _ worktree.Snapshot, stage string) {
			h.f.RunGit(stage, "checkout", "-q", "--detach")
			h.f.RunGit(h.recv, "worktree", "add", "-q", filepath.Join(h.f.Root, "elsewhere"), branch)
		}, "elsewhere"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			main := h.git(h.recv, "rev-parse", "refs/heads/main")
			if main != snap.Head.Commit {
				t.Fatalf("receiver main %s, want the snapshot head %s", main, snap.Head.Commit)
			}
			before := h.receiverState()
			dest := filepath.Join(h.f.Root, "recovered")
			p := h.pauseGit(`*" update-ref --stdin "*`, "delete "+worktree.RecoveryPrefix)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := h.restoreAsync(ctx, h.store, snap, worktree.RestoreOptions{Dest: dest, Branch: branch})
			p.wait(done)
			tt.race(h, snap, h.worktreeOn(branch))
			cancel()
			p.release()
			if res := <-done; !errors.Is(res.err, context.Canceled) {
				t.Fatalf("restore cancelled during its final cleanup = %+v, %v; want context.Canceled", res.r, res.err)
			}
			want := maps.Clone(before.heads)
			want["refs/heads/"+branch] = snap.Head.Commit
			if got := h.refs([]string{"-C", h.recv}, "refs/heads/"); !maps.Equal(got, want) {
				t.Fatalf("branches after rollback = %v, want %v", got, want)
			}
			if tt.other != "" && h.worktreeOn(branch) != filepath.Join(h.f.Root, tt.other) {
				t.Fatalf("rollback moved %s off the other worktree", branch)
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback left %s: %v", dest, err)
			}
			h.assertNoStaging(h.f.Root)
		})
	}
}

func TestRestoreReportsCancellation(t *testing.T) {
	errInjected := errors.New("injected artifact failure")
	tests := []struct {
		name  string
		match string
		fail  bool
	}{
		{"version check", `*" version "*`, false},
		{"branch selection", `*" check-ref-format "*`, false},
		{"rollback", `*" worktree remove "*`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			before := h.receiverState()
			dest := filepath.Join(h.f.Root, "recovered")
			var src worktree.ArtifactSource = h.art
			if tt.fail {
				src = interruptedSource{ArtifactSource: h.art, dest: dest, interrupt: func() error { return errInjected }}
			}
			p := h.pauseGit(tt.match, "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan restoreResult, 1)
			go func() {
				r, err := h.store.Restore(ctx, h.recvReg(), snap, src, worktree.RestoreOptions{Dest: dest})
				done <- restoreResult{r, err}
			}()
			p.wait(done)
			cancel()
			p.release()
			res := <-done
			if !errors.Is(res.err, context.Canceled) || tt.fail != errors.Is(res.err, errInjected) {
				t.Fatalf("restore cancelled during its %s = %+v, %v; want context.Canceled", tt.name, res.r, res.err)
			}
			h.assertRolledBack(before, dest)
		})
	}
}

func TestRestoreNeverReusesAStagedWorktree(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())
	other, err := worktree.OpenStore(filepath.Join(h.f.Root, "store-b"))
	if err != nil {
		t.Fatal(err)
	}
	p := h.pauseGit(`*" update-ref --stdin "*`, "delete "+worktree.RecoveryPrefix)
	a := filepath.Join(h.f.Root, "a")
	done := h.restoreAsync(t.Context(), h.store, snap, worktree.RestoreOptions{Dest: a})
	p.wait(done)
	b := filepath.Join(h.f.Root, "b")
	rb, err := other.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: b})
	if err != nil || rb.Reused || rb.Path != b {
		t.Fatalf("restore beside a staged checkout = %+v, %v; want a fresh checkout at %s", rb, err, b)
	}
	p.release()
	ra := <-done
	if ra.err != nil || ra.r.Path != a {
		t.Fatalf("staged restore = %+v, %v; want it published at %s", ra.r, ra.err, a)
	}
	h.assertFaithful(ra.r)
	h.assertFaithful(rb)
}
