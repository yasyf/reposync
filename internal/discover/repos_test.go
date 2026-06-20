package discover

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

// reposHarness is a temp-dir test rig: a default_location populated with real
// git repos, plain dirs, and probe-failing entries, scanned by Repos.
type reposHarness struct {
	t       *testing.T
	dataLoc string
}

func newReposHarness(t *testing.T) *reposHarness {
	t.Helper()
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	dataLoc := filepath.Join(root, "data")
	if err := os.MkdirAll(dataLoc, 0o750); err != nil {
		t.Fatalf("mkdir data loc: %v", err)
	}
	return &reposHarness{t: t, dataLoc: dataLoc}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not installed: %v", err)
	}
}

func (h *reposHarness) child(name string) string {
	h.t.Helper()
	dir := filepath.Join(h.dataLoc, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		h.t.Fatalf("mkdir %s: %v", name, err)
	}
	return dir
}

// gitRepo initializes a child git repo; when origin is non-empty it adds it as
// the origin remote.
func (h *reposHarness) gitRepo(name, origin string) string {
	h.t.Helper()
	dir := h.child(name)
	h.runGit(dir, "init", "-q", "-b", "main")
	if origin != "" {
		h.runGit(dir, "remote", "add", "origin", origin)
	}
	return dir
}

func (h *reposHarness) runGit(dir string, args ...string) string {
	h.t.Helper()
	//nolint:gosec // G204: test helper running git with test-controlled args against a temp repo.
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *reposHarness) state(repos ...state.Repo) *state.State {
	h.t.Helper()
	return &state.State{DefaultLocation: h.dataLoc, Repos: repos}
}

func candidateFor(t *testing.T, candidates []Candidate, relpath string) Candidate {
	t.Helper()
	for _, c := range candidates {
		if c.Relpath == relpath {
			return c
		}
	}
	t.Fatalf("no candidate for relpath %q in %+v", relpath, candidates)
	return Candidate{}
}

func hasName(notes []SkipNote, name string) bool {
	for _, n := range notes {
		if n.Name == name {
			return true
		}
	}
	return false
}

func hasCandidate(candidates []Candidate, relpath string) bool {
	for _, c := range candidates {
		if c.Relpath == relpath {
			return true
		}
	}
	return false
}

func TestReposClassifiesChildren(t *testing.T) {
	h := newReposHarness(t)
	const origin = "https://example.com/foo.git"
	h.gitRepo("withorigin", origin)
	h.gitRepo("noremote", "")
	h.child("plaindir")

	// A repo whose .git is unreadable forces a probe error: skipped-with-note,
	// scan not aborted. Restore the mode so t.TempDir cleanup can remove it.
	brokenDir := h.gitRepo("broken", "")
	brokenGit := filepath.Join(brokenDir, ".git")
	if err := os.Chmod(brokenGit, 0o000); err != nil {
		t.Fatalf("chmod broken .git: %v", err)
	}
	//nolint:gosec // G302: brokenGit is a directory; 0o750 restores traversable perms for test cleanup.
	t.Cleanup(func() { _ = os.Chmod(brokenGit, 0o750) })

	st := h.state(state.Repo{Relpath: "withorigin", Origin: origin, Trunk: "main"})

	result, err := Repos(context.Background(), st)
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}

	withOrigin := candidateFor(t, result.Candidates, "withorigin")
	if withOrigin.Kind != "git" {
		t.Fatalf("withorigin kind = %q, want git", withOrigin.Kind)
	}
	if withOrigin.Origin != origin {
		t.Fatalf("withorigin origin = %q, want %q", withOrigin.Origin, origin)
	}
	if withOrigin.LocalOnly {
		t.Fatal("withorigin LocalOnly = true, want false")
	}
	if !withOrigin.Tracked {
		t.Fatal("withorigin Tracked = false, want true")
	}
	if withOrigin.AbsPath != filepath.Join(h.dataLoc, "withorigin") {
		t.Fatalf("withorigin abspath = %q", withOrigin.AbsPath)
	}

	noRemote := candidateFor(t, result.Candidates, "noremote")
	if noRemote.Kind != "git" {
		t.Fatalf("noremote kind = %q, want git", noRemote.Kind)
	}
	if !noRemote.LocalOnly {
		t.Fatal("noremote LocalOnly = false, want true")
	}
	if noRemote.Origin != "" {
		t.Fatalf("noremote origin = %q, want empty", noRemote.Origin)
	}
	if noRemote.Tracked {
		t.Fatal("noremote Tracked = true, want false")
	}

	if hasCandidate(result.Candidates, "plaindir") {
		t.Fatal("plaindir present in Candidates; non-repo dir must be dropped")
	}
	if hasName(result.Skipped, "plaindir") {
		t.Fatal("plaindir present in Skipped; non-repo dir is a silent skip")
	}

	if !hasName(result.Skipped, "broken") {
		t.Fatalf("broken not in Skipped: %+v", result.Skipped)
	}
	if hasCandidate(result.Candidates, "broken") {
		t.Fatal("broken present in Candidates; probe failure must skip-with-note")
	}
}

func TestReposSortedByRelpath(t *testing.T) {
	h := newReposHarness(t)
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		h.gitRepo(name, "")
	}
	result, err := Repos(context.Background(), h.state())
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	got := make([]string, len(result.Candidates))
	for i, c := range result.Candidates {
		got[i] = c.Relpath
	}
	want := []string{"alpha", "bravo", "charlie"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidate order = %v, want %v", got, want)
	}
}

func TestReposSkipsDotfilesAndTmpDir(t *testing.T) {
	h := newReposHarness(t)
	h.gitRepo(".hidden", "")
	h.gitRepo(reconcile.TmpDirName, "")
	h.gitRepo("visible", "")

	result, err := Repos(context.Background(), h.state())
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].Relpath != "visible" {
		t.Fatalf("candidates = %+v, want only visible", result.Candidates)
	}
}

func TestReposLocalOnlyTrackedByRelpath(t *testing.T) {
	h := newReposHarness(t)
	h.gitRepo("localtracked", "")
	st := h.state(state.Repo{Relpath: "localtracked", Origin: "", Trunk: "main", LocalOnly: true})

	result, err := Repos(context.Background(), st)
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	c := candidateFor(t, result.Candidates, "localtracked")
	if !c.LocalOnly {
		t.Fatal("localtracked LocalOnly = false, want true")
	}
	if !c.Tracked {
		t.Fatal("localtracked Tracked = false, want true (matched on relpath)")
	}
}

func TestReposMissingDefaultLocation(t *testing.T) {
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	st := &state.State{DefaultLocation: filepath.Join(root, "does-not-exist")}

	result, err := Repos(context.Background(), st)
	if err != nil {
		t.Fatalf("Repos on missing default location: %v", err)
	}
	if len(result.Candidates) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("missing default location yielded %+v, want empty", result)
	}
}

func TestReposSkipsNonDirSymlinkAndFollowsDirSymlink(t *testing.T) {
	h := newReposHarness(t)
	target := h.gitRepo("realrepo", "")

	// A symlink to a directory repo should be followed and classified.
	dirLink := filepath.Join(h.dataLoc, "linkrepo")
	if err := os.Symlink(target, dirLink); err != nil {
		t.Fatalf("symlink to dir: %v", err)
	}
	// A symlink to a plain file is not a directory and must be skipped silently.
	plainFile := filepath.Join(h.dataLoc, "afile")
	if err := os.WriteFile(plainFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write plain file: %v", err)
	}
	fileLink := filepath.Join(h.dataLoc, "linkfile")
	if err := os.Symlink(plainFile, fileLink); err != nil {
		t.Fatalf("symlink to file: %v", err)
	}

	result, err := Repos(context.Background(), h.state())
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if !hasCandidate(result.Candidates, "linkrepo") {
		t.Fatalf("linkrepo (dir symlink) not classified: %+v", result.Candidates)
	}
	if hasCandidate(result.Candidates, "linkfile") || hasName(result.Skipped, "linkfile") {
		t.Fatal("linkfile (file symlink) must be skipped silently")
	}
	if hasCandidate(result.Candidates, "afile") || hasName(result.Skipped, "afile") {
		t.Fatal("afile (plain file) must be skipped silently")
	}
}
