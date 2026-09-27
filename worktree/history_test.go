package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func TestCaptureRebuildsMissingHistoryBundleBeforeAppend(t *testing.T) {
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	trunk := strings.TrimSpace(f.RunGit(repo, "rev-parse", "origin/main"))
	commit := func(name string) string {
		f.WriteFile(repo, name, name+"\n")
		f.RunGit(repo, "add", name)
		f.RunGit(repo, "commit", "-qm", name)
		return strings.TrimSpace(f.RunGit(repo, "rev-parse", "HEAD"))
	}
	commit("a.txt")
	wt := discoverAt(t, repo, repo)
	st, sink := openStore(t), worktreetest.New()
	first := mustCapture(t, st, wt, sink)
	if len(first.History) != 1 {
		t.Fatalf("first history has %d links, want 1", len(first.History))
	}
	sink.Remove(first.History[0].Artifact)

	tip := commit("b.txt")
	second := mustCapture(t, st, wt, sink)
	if !second.Complete || len(second.History) != 1 || second.History[0].Tip != tip || !slices.Equal(second.History[0].Prerequisites, []string{trunk}) || !slices.Equal(second.Requires, []string{trunk}) {
		t.Fatalf("second capture: complete=%v history=%+v requires=%v, want rebuilt link at %s from %s", second.Complete, second.History, second.Requires, tip, trunk)
	}
	has, err := sink.Has(context.Background(), []worktree.ArtifactRef{second.History[0].Artifact})
	if err != nil || !has[0] {
		t.Fatalf("rebuilt history held=%v err=%v, want true", has, err)
	}
}

func TestCaptureRebuildsAfterHistoryPrerequisiteIsPruned(t *testing.T) {
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	oldTrunk := strings.TrimSpace(f.RunGit(repo, "rev-parse", "origin/main"))
	f.WriteFile(repo, "old-work.txt", "old work\n")
	f.RunGit(repo, "add", "old-work.txt")
	f.RunGit(repo, "commit", "-qm", "old work")
	wt := discoverAt(t, repo, repo)
	st, sink := openStore(t), worktreetest.New()
	first := mustCapture(t, st, wt, sink)
	if !slices.Equal(first.Requires, []string{oldTrunk}) {
		t.Fatalf("first requires %v, want [%s]", first.Requires, oldTrunk)
	}

	f.RunGit(repo, "checkout", "-q", "--orphan", "replacement")
	f.RunGit(repo, "rm", "-qrf", ".")
	f.WriteFile(repo, "root.txt", "new root\n")
	f.RunGit(repo, "add", "root.txt")
	f.RunGit(repo, "commit", "-qm", "new root")
	newTrunk := strings.TrimSpace(f.RunGit(repo, "rev-parse", "HEAD"))
	f.RunGit(repo, "push", "-qf", "origin", "HEAD:main")
	f.RunGit(repo, "fetch", "-q", "origin")
	f.WriteFile(repo, "new-work.txt", "new work\n")
	f.RunGit(repo, "add", "new-work.txt")
	f.RunGit(repo, "commit", "-qm", "new work")
	tip := strings.TrimSpace(f.RunGit(repo, "rev-parse", "HEAD"))
	f.RunGit(repo, "branch", "-D", "main")
	f.RunGit(repo, "reflog", "expire", "--expire=now", "--all")
	f.RunGit(repo, "gc", "--prune=now")

	//nolint:gosec // G204: git against a test-controlled temp repo; the lookup must fail.
	cmd := exec.Command("git", "cat-file", "-e", oldTrunk+"^{commit}")
	cmd.Dir = repo
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		t.Fatalf("pruned prerequisite cat-file error = %v, want exit 128", err)
	}

	second := mustCapture(t, st, wt, sink)
	if !second.Complete || len(second.History) != 1 || second.History[0].Tip != tip || !slices.Equal(second.History[0].Prerequisites, []string{newTrunk}) || !slices.Equal(second.Requires, []string{newTrunk}) {
		t.Fatalf("second capture: complete=%v history=%+v requires=%v, want rebuilt link at %s from %s", second.Complete, second.History, second.Requires, tip, newTrunk)
	}
}

func TestCaptureSurfacesUnreadableHistoryPrerequisite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 objects")
	}
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	oldTrunk := strings.TrimSpace(f.RunGit(repo, "rev-parse", "origin/main"))
	f.WriteFile(repo, "old-work.txt", "old work\n")
	f.RunGit(repo, "add", "old-work.txt")
	f.RunGit(repo, "commit", "-qm", "old work")
	wt := discoverAt(t, repo, repo)
	st, sink := openStore(t), worktreetest.New()
	first := mustCapture(t, st, wt, sink)
	if !slices.Equal(first.Requires, []string{oldTrunk}) {
		t.Fatalf("first requires %v, want [%s]", first.Requires, oldTrunk)
	}

	f.RunGit(repo, "checkout", "-q", "--orphan", "replacement")
	f.RunGit(repo, "rm", "-qrf", ".")
	f.WriteFile(repo, "root.txt", "new root\n")
	f.RunGit(repo, "add", "root.txt")
	f.RunGit(repo, "commit", "-qm", "new root")
	f.RunGit(repo, "push", "-qf", "origin", "HEAD:main")
	f.RunGit(repo, "fetch", "-q", "origin")
	f.WriteFile(repo, "new-work.txt", "new work\n")
	f.RunGit(repo, "add", "new-work.txt")
	f.RunGit(repo, "commit", "-qm", "new work")

	loose := filepath.Join(repo, ".git", "objects", oldTrunk[:2], oldTrunk[2:])
	if err := os.Chmod(loose, 0); err != nil {
		t.Fatalf("chmod prerequisite object: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(loose, 0o400); err != nil {
			t.Errorf("restore prerequisite object: %v", err)
		}
	})

	snap, err := capture(t, st, wt, sink, worktree.Limits{})
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("second capture: complete=%v err=%v, want permission-denied error", snap.Complete, err)
	}
}

func TestCaptureRebuildsPastUnreadableHistoryPack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 pack indexes")
	}
	type replaced struct {
		rt                      *roundTrip
		wt                      worktree.Worktree
		first                   worktree.Snapshot
		oldTrunk, newTrunk, tip string
	}
	replaceTrunk := func(t *testing.T) replaced {
		t.Helper()
		f := vcstest.New(t)
		src := f.GitClone(filepath.Join(f.Root, "src"))
		r := replaced{rt: newRoundTrip(t, f, src, filepath.Join(f.Root, "recv"))}
		r.oldTrunk = r.rt.git(src, "rev-parse", "origin/main")
		r.rt.commit(src, "old-work.txt", "old work\n")
		r.wt = discoverAt(t, src, src)
		r.first = mustCapture(t, r.rt.srcStore, r.wt, r.rt.art)
		if len(r.first.History) != 1 || !slices.Equal(r.first.Requires, []string{r.oldTrunk}) {
			t.Fatalf("first capture: history=%+v requires=%v, want one link requiring %s", r.first.History, r.first.Requires, r.oldTrunk)
		}
		f.RunGit(src, "checkout", "-q", "--orphan", "replacement")
		f.RunGit(src, "rm", "-qrf", ".")
		r.newTrunk = r.rt.commit(src, "root.txt", "new root\n")
		f.RunGit(src, "push", "-qf", "origin", "HEAD:main")
		f.RunGit(src, "fetch", "-q", "origin")
		r.tip = r.rt.commit(src, "new-work.txt", "new work\n")
		return r
	}

	t.Run("pack holding rebuilt objects fails capture", func(t *testing.T) {
		r := replaceTrunk(t)
		packUnreadable(t, r.rt.src, r.oldTrunk, r.rt.git(r.rt.src, "rev-parse", r.tip+":new-work.txt"))

		snap, err := capture(t, r.rt.srcStore, r.wt, r.rt.art, worktree.Limits{})
		if err == nil || !strings.Contains(err.Error(), "bundle create") {
			t.Fatalf("capture: complete=%v history=%+v err=%v, want the rebuilt bundle to fail on the unreadable blob", snap.Complete, snap.History, err)
		}
	})

	t.Run("pack holding only the obsolete prerequisite rebuilds and restores", func(t *testing.T) {
		r := replaceTrunk(t)
		want := expectation{files: r.rt.worktreeFiles(r.rt.src), status: r.rt.status(r.rt.src), log: r.rt.git(r.rt.src, "log", "--format=%H")}
		packUnreadable(t, r.rt.src, r.oldTrunk)

		second := mustCapture(t, r.rt.srcStore, r.wt, r.rt.art)
		if !second.Complete || len(second.History) != 1 || second.History[0].Tip != r.tip || second.History[0].Artifact == r.first.History[0].Artifact ||
			!slices.Equal(second.History[0].Prerequisites, []string{r.newTrunk}) || !slices.Equal(second.Requires, []string{r.newTrunk}) {
			t.Fatalf("second capture: complete=%v history=%+v requires=%v, want a freshly cut link at %s from %s", second.Complete, second.History, second.Requires, r.tip, r.newTrunk)
		}
		enc, err := worktree.Encode(second)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := worktree.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}

		r.rt.f.RunGit(r.rt.f.Root, "clone", "-q", "--no-local", r.rt.f.Origin, r.rt.recv)
		r.rt.f.ConfigGit(r.rt.recv)
		if code, out := catFileExists(t, r.rt.recv, r.oldTrunk); code != 1 {
			t.Fatalf("receiver cat-file -e %s = exit %d %q, want the obsolete prerequisite absent", r.oldTrunk, code, out)
		}
		dest := filepath.Join(r.rt.f.Root, "recovered")
		r.rt.assertRestored(snap, r.rt.pickup(r.rt.store, r.rt.recv, r.rt.art, snap, worktree.RestoreOptions{Dest: dest}), want)
	})
}

func packUnreadable(t *testing.T, repo string, oids ...string) {
	t.Helper()
	objects := filepath.Join(repo, ".git", "objects")
	//nolint:gosec // G204: git against a test-controlled temp repo.
	cmd := exec.Command("git", "pack-objects", "-q", filepath.Join(objects, "pack", "pack"))
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pack-objects %v: %v", oids, err)
	}
	for _, oid := range oids {
		if err := os.Remove(filepath.Join(objects, oid[:2], oid[2:])); err != nil {
			t.Fatalf("drop loose %s: %v", oid, err)
		}
	}
	idx := filepath.Join(objects, "pack", "pack-"+strings.TrimSpace(string(out))+".idx")
	if err := os.Chmod(idx, 0); err != nil {
		t.Fatalf("chmod pack index: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(idx, 0o400); err != nil {
			t.Errorf("restore pack index: %v", err)
		}
	})
	for _, oid := range oids {
		if code, out := catFileExists(t, repo, oid); code != 1 || out != "" {
			t.Fatalf("cat-file -e %s = exit %d %q, want a silent exit 1", oid, code, out)
		}
	}
}

func catFileExists(t *testing.T, repo, oid string) (int, string) {
	t.Helper()
	//nolint:gosec // G204: git against a test-controlled temp repo.
	cmd := exec.Command("git", "cat-file", "-e", oid)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("cat-file -e %s: %v", oid, err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}
