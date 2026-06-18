package vcs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openJJ(t *testing.T, path string) Repo {
	t.Helper()
	r, err := Open(path, "main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r.Kind() != "jj" {
		t.Fatalf("kind = %q, want jj", r.Kind())
	}
	return r
}

func TestJJHasTrunkAndOrigin(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)

	ok, err := r.HasTrunk(context.Background())
	if err != nil {
		t.Fatalf("has trunk: %v", err)
	}
	if !ok {
		t.Fatal("HasTrunk = false, want true on colocated clone")
	}
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

func TestJJAdvanceIdleEmpty(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)
	want := f.advanceOrigin("v2")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeAdvanced {
		t.Fatalf("outcome = %q, want advanced", got)
	}
	// @ is empty and sits on the new trunk.
	probe := f.runJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy",
		"-T", `"empty=" ++ empty ++ " parent=" ++ parents.map(|c| c.commit_id()).join(",") ++ "\n"`)
	probe = strings.TrimSpace(probe)
	if !strings.HasPrefix(probe, "empty=true ") {
		t.Fatalf("@ not empty after advance: %q", probe)
	}
	if !strings.Contains(probe, "parent="+want) {
		t.Fatalf("@ parent != new trunk %q: %q", want, probe)
	}
	if h, _ := r.TrunkHash(context.Background()); h != want {
		t.Fatalf("trunk hash = %q, want %q", h, want)
	}
}

func TestJJAdvanceUpToDate(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeUpToDate {
		t.Fatalf("outcome = %q, want up-to-date", got)
	}
}

func TestJJInUseDirtyNoClobber(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)

	// A real edit plus the snapshot a normal jj command takes; the poller never
	// snapshots, so the file must be recorded by genuine activity to be seen.
	f.writeFile(dest, "WORK.txt", "in progress\n")
	f.snapshotJJ(dest)

	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on dirty @")
	}
	if reason != "dirty working copy" {
		t.Fatalf("reason = %q, want dirty working copy", reason)
	}

	opsBefore := f.runJJ(dest, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `id.short() ++ "\n"`)
	// Advance must return not-disposable and leave the change and op log intact.
	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable", got)
	}
	if !f.fileExists(dest, "WORK.txt") {
		t.Fatal("in-progress file was clobbered")
	}
	if got := f.readFile(dest, "WORK.txt"); got != "in progress\n" {
		t.Fatalf("in-progress file content changed to %q", got)
	}
	opsAfter := f.runJJ(dest, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `id.short() ++ "\n"`)
	if !strings.Contains(opsAfter, strings.SplitN(strings.TrimSpace(opsBefore), "\n", 2)[0]) {
		t.Fatal("op log head changed: op history was disturbed")
	}
}

// TestJJInUseUnsnapshottedDirty edits a tracked file on disk and calls InUse with
// NO intervening jj command. The gate must snapshot the live edit into @ and report
// busy; with --ignore-working-copy on the dirty check it would miss the edit.
func TestJJInUseUnsnapshottedDirty(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)

	// Modify a tracked file directly on disk; run no jj command before InUse.
	f.writeFile(dest, "README.md", "hello\nedited but not snapshotted\n")

	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy on unsnapshotted on-disk edit")
	}
	if reason != "dirty working copy" {
		t.Fatalf("reason = %q, want dirty working copy", reason)
	}
}

// TestJJUnsnapshottedNoClobber proves the no-clobber guarantee over the
// unsnapshotted window: a tracked file is edited on disk (no jj command), origin
// advances, and the sync-level InUse->skip path leaves the edit untouched on disk
// and retained once @ is snapshotted.
func TestJJUnsnapshottedNoClobber(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)

	const edited = "hello\nlive edit\n"
	f.writeFile(dest, "README.md", edited)
	f.advanceOrigin("v2")

	// Mirror syncOne: InUse busy => caller skips, Advance is never reached.
	busy, reason, err := r.InUse(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("in use: %v", err)
	}
	if !busy {
		t.Fatal("InUse = false, want busy so the repo is skipped")
	}
	if reason != "dirty working copy" {
		t.Fatalf("reason = %q, want dirty working copy", reason)
	}

	// The edit is intact on disk and retained in @ after a snapshot.
	if got := f.readFile(dest, "README.md"); got != edited {
		t.Fatalf("README.md on disk = %q, want %q", got, edited)
	}
	diff := f.runJJ(dest, "diff", "-r", "@", "--git")
	if !strings.Contains(diff, "live edit") {
		t.Fatalf("jj diff does not retain the live edit:\n%s", diff)
	}
}

// TestJJAdvanceUpdatesWorkingCopy proves jj new advances the working copy on disk:
// a tracked file added on new main must appear with new-main content after Advance,
// not leave a stale working copy at old main.
func TestJJAdvanceUpdatesWorkingCopy(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)

	// Origin gains a new tracked file that does not exist in the clone's working copy.
	f.writeFile(f.seed, "NEW.txt", "from new main\n")
	f.runGit(f.seed, "add", "NEW.txt")
	f.runGit(f.seed, "commit", "-qm", "add NEW.txt")
	f.runGit(f.seed, "push", "-q", "origin", "main")
	if f.fileExists(dest, "NEW.txt") {
		t.Fatal("NEW.txt present before advance: fixture precondition broken")
	}

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeAdvanced {
		t.Fatalf("outcome = %q, want advanced", got)
	}
	if !f.fileExists(dest, "NEW.txt") {
		t.Fatal("NEW.txt absent after advance: working copy left stale")
	}
	if c := f.readFile(dest, "NEW.txt"); c != "from new main\n" {
		t.Fatalf("NEW.txt content = %q, want new-main content", c)
	}
}

func TestJJAdvanceNotDisposableWithDescription(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)
	f.advanceOrigin("v2")

	changeBefore := f.runJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`)
	f.runJJ(dest, "describe", "-m", "work in progress", "--ignore-working-copy")

	got, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != OutcomeNotDisposable {
		t.Fatalf("outcome = %q, want not-disposable", got)
	}
	changeAfter := f.runJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `change_id`)
	if changeBefore != changeAfter {
		t.Fatalf("@ moved from %q to %q, want unchanged", changeBefore, changeAfter)
	}
	desc := strings.TrimSpace(f.runJJ(dest, "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `description.first_line()`))
	if desc != "work in progress" {
		t.Fatalf("description = %q, want preserved", desc)
	}
}

func TestJJNoOrigin(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(f.root, "noorigin")
	f.jjInit(dest)
	r := openJJ(t, dest)

	_, err := r.Origin(context.Background())
	if !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("origin err = %v, want ErrNoOrigin", err)
	}
	ok, err := r.HasTrunk(context.Background())
	if err != nil {
		t.Fatalf("has trunk: %v", err)
	}
	if ok {
		t.Fatal("HasTrunk = true, want false on remoteless repo")
	}
}

func TestJJNeverPushOnAdvance(t *testing.T) {
	f := newFixture(t)
	dest := f.jjClone(filepath.Join(f.root, "clone"))
	r := openJJ(t, dest)
	f.advanceOrigin("v2")

	originBefore := f.originMain()
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if originBefore != f.originMain() {
		t.Fatalf("NEVER-PUSH violated: origin main moved from %q to %q", originBefore, f.originMain())
	}
}
