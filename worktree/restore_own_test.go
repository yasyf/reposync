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

const tempCleanup = `*" update-ref"*" --stdin "*`

func TestRestoreRollbackSparesTakenOverBranch(t *testing.T) {
	const branch = "recovery/raced"
	tests := []struct {
		name  string
		race  func(h *harness, head string)
		other string
	}{
		{"symbolic ref", func(h *harness, _ string) {
			h.f.RunGit(h.recv, "symbolic-ref", "refs/heads/"+branch, "refs/heads/main")
		}, ""},
		{"created at the snapshot head", func(h *harness, head string) {
			h.f.RunGit(h.recv, "branch", branch, head)
		}, ""},
		{"checked out elsewhere", func(h *harness, head string) {
			h.f.RunGit(h.recv, "worktree", "add", "-q", "-b", branch, filepath.Join(h.f.Root, "elsewhere"), head)
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
			p := h.pauseGit(`*" worktree remove "*`, "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := interruptedSource{ArtifactSource: h.art, dest: dest, interrupt: func() error { cancel(); return nil }}
			done := make(chan restoreResult, 1)
			go func() {
				r, err := h.store.Restore(ctx, h.recvReg(), snap, src, worktree.RestoreOptions{Dest: dest, Branch: branch})
				done <- restoreResult{r, err}
			}()
			p.wait(done)
			if got := h.refs([]string{"-C", h.recv}, "refs/heads/"); !maps.Equal(got, before.heads) {
				t.Fatalf("branches while rolling back = %v, want the untouched %v", got, before.heads)
			}
			tt.race(h, snap.Head.Commit)
			p.release()
			if res := <-done; !errors.Is(res.err, context.Canceled) {
				t.Fatalf("restore cancelled mid-materialize = %+v, %v; want context.Canceled", res.r, res.err)
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

func TestRestoreSuffixesBranchTakenWhileStaged(t *testing.T) {
	const branch = "recovery/raced"
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())
	dest := filepath.Join(h.f.Root, "recovered")
	p := h.pauseGit(`*" fetch "*":refs/reposync/recovery/"*`, "")
	done := h.restoreAsync(t.Context(), h.store, snap, worktree.RestoreOptions{Dest: dest, Branch: branch})
	p.wait(done)
	h.f.RunGit(h.recv, "branch", branch, snap.Head.Commit)
	p.release()
	res := <-done
	if res.err != nil || res.r.Path != dest || res.r.Branch != branch+"-2" {
		t.Fatalf("restore racing a branch = %+v, %v; want %s-2 published at %s", res.r, res.err, branch, dest)
	}
	h.assertFaithful(res.r)
	if got := h.worktreeOn(branch + "-2"); got != dest {
		t.Fatalf("%s-2 is checked out at %s, want %s", branch, got, dest)
	}
	if got := h.refs([]string{"-C", h.recv}, "refs/heads/")["refs/heads/"+branch]; got != snap.Head.Commit {
		t.Fatalf("user's %s = %q, want it untouched at %s", branch, got, snap.Head.Commit)
	}
}

func TestRestoreRollbackKeepsReplacedWorktree(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancelled %v", cancelled), func(t *testing.T) {
			h := newGitHarness(t)
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			dest := filepath.Join(h.f.Root, "recovered")
			p := h.pauseGit(tempCleanup, "delete "+worktree.RecoveryPrefix)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := h.restoreAsync(ctx, h.store, snap, worktree.RestoreOptions{Dest: dest})
			p.wait(done)
			stage := h.staged(dest)
			moved := filepath.Join(h.f.Root, "moved")
			h.f.RunGit(h.recv, "worktree", "move", stage, moved)
			h.f.RunGit(h.recv, "worktree", "add", "-q", "-b", "replacement", stage, snap.Head.Commit)
			h.f.WriteFile(stage, "mine.txt", "user work\n")
			if cancelled {
				cancel()
			}
			p.release()
			if res := <-done; res.err == nil || cancelled != errors.Is(res.err, context.Canceled) {
				t.Fatalf("restore whose staged checkout was replaced = %+v, %v; want a failure, cancelled %v", res.r, res.err, cancelled)
			}
			if got := h.worktreeOn("replacement"); got != stage || h.f.ReadFile(stage, "mine.txt") != "user work\n" {
				t.Fatalf("replacement worktree at %s after rollback, want it and mine.txt kept at %s", got, stage)
			}
			if _, err := os.Lstat(filepath.Join(moved, ".git")); err != nil {
				t.Fatalf("rollback removed the moved checkout: %v", err)
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback left %s: %v", dest, err)
			}
		})
	}
}

func TestRestoreCleanupNeverFollowsTemporarySymref(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())
	if main := h.git(h.recv, "rev-parse", "refs/heads/main"); main != snap.Head.Commit {
		t.Fatalf("receiver main %s, want the snapshot head %s", main, snap.Head.Commit)
	}
	before := h.receiverState()
	dest := filepath.Join(h.f.Root, "recovered")
	p := h.pauseGit(tempCleanup, "delete "+worktree.RecoveryPrefix)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.restoreAsync(ctx, h.store, snap, worktree.RestoreOptions{Dest: dest})
	p.wait(done)
	var head string
	for ref := range h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix) {
		if strings.HasSuffix(ref, "/head") {
			head = ref
		}
	}
	h.f.RunGit(h.recv, "symbolic-ref", head, "refs/heads/main")
	cancel()
	p.release()
	if res := <-done; !errors.Is(res.err, context.Canceled) {
		t.Fatalf("restore cancelled during its final cleanup = %+v, %v; want context.Canceled", res.r, res.err)
	}
	h.assertRolledBack(before, dest)
}

func TestRestorePublishSparesReplacedClaim(t *testing.T) {
	h, snap, lfs := lfsFetchHarness(t)
	before := h.receiverState()
	dest := filepath.Join(h.f.Root, "recovered")
	swapped := make(chan os.FileInfo, 1)
	gate := func(ctx context.Context, fetch func(context.Context) error) error {
		fetchCtx, stop := context.WithCancel(ctx)
		defer stop()
		go func() {
			select {
			case <-lfs.started:
				if err := os.Rename(dest, dest+"-claimed"); err != nil {
					t.Errorf("move claim aside: %v", err)
				}
				if err := os.Mkdir(dest, 0o700); err != nil {
					t.Errorf("replace claim: %v", err)
				}
				info, err := os.Lstat(dest)
				if err != nil {
					t.Errorf("stat replacement: %v", err)
				}
				swapped <- info
				stop()
			case <-fetchCtx.Done():
			}
		}()
		return fmt.Errorf("%w: %w", worktree.ErrFetchDeferred, fetch(fetchCtx))
	}
	r, err := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: dest, FetchLFS: gate})
	if !errors.Is(err, worktree.ErrDestinationExists) {
		t.Fatalf("restore whose claim was replaced = %+v, %v; want ErrDestinationExists", r, err)
	}
	var replacement os.FileInfo
	select {
	case replacement = <-swapped:
	default:
		t.Fatal("the claim was never replaced")
	}
	info, err := os.Lstat(dest)
	if err != nil || !os.SameFile(info, replacement) {
		t.Fatalf("dest after publish = %v, %v; want the replacement directory", info, err)
	}
	if entries, err := os.ReadDir(dest); err != nil || len(entries) != 0 {
		t.Fatalf("replacement holds %v, %v; want it empty", entries, err)
	}
	after := h.receiverState()
	if !maps.Equal(before.heads, after.heads) || before.worktrees != after.worktrees || !slices.Equal(before.status, after.status) {
		t.Fatalf("receiver changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
		t.Fatalf("recovery refs left in checkout: %v", refs)
	}
	h.assertNoStaging(h.f.Root)
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
	p := h.pauseGit(tempCleanup, "delete "+worktree.RecoveryPrefix)
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
