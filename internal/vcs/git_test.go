package vcs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openGit(t *testing.T, path string) Repo {
	t.Helper()
	r, err := Open(path, "main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r.Kind() != "git" {
		t.Fatalf("kind = %q, want git", r.Kind())
	}
	return r
}

func TestGitOpenAndOrigin(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	origin, err := r.Origin(context.Background())
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	if origin != f.origin {
		t.Fatalf("origin = %q, want %q", origin, f.origin)
	}
	hash, err := r.TrunkHash(context.Background())
	if err != nil {
		t.Fatalf("trunk hash: %v", err)
	}
	if hash != f.originMain() {
		t.Fatalf("trunk hash = %q, want %q", hash, f.originMain())
	}
}

func TestGitHasTrunk(t *testing.T) {
	f := newFixture(t)
	r := openGit(t, f.gitClone(filepath.Join(f.root, "clone")))
	ok, err := r.HasTrunk(context.Background())
	if err != nil {
		t.Fatalf("has trunk: %v", err)
	}
	if !ok {
		t.Fatal("HasTrunk = false, want true")
	}
}

func TestGitAdvance(t *testing.T) {
	t.Run("clean up-to-date returns up-to-date", func(t *testing.T) {
		f := newFixture(t)
		r := openGit(t, f.gitClone(filepath.Join(f.root, "clone")))
		got, err := r.Advance(context.Background())
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if got != OutcomeUpToDate {
			t.Fatalf("outcome = %q, want up-to-date", got)
		}
	})

	t.Run("on trunk behind advances via ff", func(t *testing.T) {
		f := newFixture(t)
		dest := f.gitClone(filepath.Join(f.root, "clone"))
		r := openGit(t, dest)
		want := f.advanceOrigin("v2")

		got, err := r.Advance(context.Background())
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if got != OutcomeAdvanced {
			t.Fatalf("outcome = %q, want advanced", got)
		}
		localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main"))
		if localMain != want {
			t.Fatalf("local main = %q, want origin %q", localMain, want)
		}
	})
}

func TestGitInUseDirty(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)
	f.writeFile(dest, "DIRTY.txt", "uncommitted\n")

	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on dirty tree")
	}
	if reason != "dirty working tree" {
		t.Fatalf("reason = %q, want dirty working tree", reason)
	}

	// Advance must leave the dirty file intact (it can ff-merge a clean index,
	// but the caller gates on InUse; here we assert the file survives regardless).
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !f.fileExists(dest, "DIRTY.txt") {
		t.Fatal("dirty file was clobbered")
	}
	if got := f.readFile(dest, "DIRTY.txt"); got != "uncommitted\n" {
		t.Fatalf("dirty file content changed to %q", got)
	}
}

func TestGitInUseRecentReflog(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy from recent clone reflog entry")
	}
	if reason != "recent activity" {
		t.Fatalf("reason = %q, want recent activity", reason)
	}

	notBusy, _, err := r.InUse(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if notBusy {
		t.Fatal("InUse = busy with tiny idle window, want not busy")
	}
}

func TestGitDivergedRefusesFFNoClobber(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Divergent local commit on main.
	f.writeFile(dest, "LOCAL.md", "local-only\n")
	f.runGit(dest, "add", "LOCAL.md")
	f.runGit(dest, "commit", "-qm", "local divergent")
	localHead := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD"))
	f.advanceOrigin("v2")

	originBefore := f.originMain()
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date (ff refused, no-op)", got)
	}
	if head := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD")); head != localHead {
		t.Fatalf("local HEAD moved to %q, want unchanged %q", head, localHead)
	}
	if !f.fileExists(dest, "LOCAL.md") {
		t.Fatal("divergent local file was clobbered")
	}
	if originBefore != f.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.originMain())
	}
}

func TestGitDetachedHeadAdvancesLocalTrunk(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	f.runGit(dest, "checkout", "-q", "--detach", "HEAD")
	detachedAt := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD"))
	want := f.advanceOrigin("v2")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeAdvanced {
		t.Fatalf("outcome = %q, want advanced", got)
	}
	if localMain := strings.TrimSpace(f.runGit(dest, "rev-parse", "main")); localMain != want {
		t.Fatalf("local main = %q, want origin %q", localMain, want)
	}
	if head := strings.TrimSpace(f.runGit(dest, "rev-parse", "HEAD")); head != detachedAt {
		t.Fatalf("HEAD moved to %q, want unchanged detached %q", head, detachedAt)
	}
}

func TestGitNeverPushOnAdvance(t *testing.T) {
	f := newFixture(t)
	dest := f.gitClone(filepath.Join(f.root, "clone"))
	r := openGit(t, dest)

	// Local main ahead of origin.
	f.writeFile(dest, "AHEAD.md", "ahead\n")
	f.runGit(dest, "add", "AHEAD.md")
	f.runGit(dest, "commit", "-qm", "local ahead")

	originBefore := f.originMain()
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if originBefore != f.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.originMain())
	}
}
