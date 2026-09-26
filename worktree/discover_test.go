package worktree

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/registry"
)

type discovered struct {
	Kind      Kind
	Name      string
	Branch    string
	Head      string
	GitDir    string
	CommonDir string
	Locked    bool
}

func TestDiscover(t *testing.T) {
	f := vcstest.New(t)
	repos := filepath.Join(f.Root, "repos")
	wts := filepath.Join(f.Root, "wts")
	for _, d := range []string{repos, wts, filepath.Join(f.Root, "gitdirs")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	main := f.GitClone(filepath.Join(repos, "main"))
	feat := f.LinkedWorktree(main, filepath.Join(wts, "feat"), "feat")
	locked := f.LinkedWorktree(main, filepath.Join(wts, "locked"), "")
	f.RunGit(main, "worktree", "lock", locked)
	gone := f.LinkedWorktree(main, filepath.Join(wts, "gone"), "gone")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	f.GitClone(filepath.Join(repos, "local"))
	jjMain := f.JJClone(filepath.Join(repos, "jj"))
	jjWS := f.JJWorkspace(jjMain, filepath.Join(wts, "jj\tws\nx"), "sec\tond\nx")
	sep := filepath.Join(repos, "sep")
	sepGit := filepath.Join(f.Root, "gitdirs", "sep.git")
	f.RunGit(f.Root, "clone", "-q", "--separate-git-dir", sepGit, f.Origin, sep)

	reg := registry.Registry{Repos: []registry.Repo{
		{Relpath: "jj", Path: jjMain, Origin: "https://example.com/jj.git", Trunk: "main"},
		{Relpath: "local", Path: filepath.Join(repos, "local"), Origin: "https://example.com/local.git", Trunk: "main", LocalOnly: true},
		{Relpath: "main", Path: main, Origin: "https://example.com/main.git", Trunk: "main"},
		{Relpath: "missing", Path: filepath.Join(repos, "missing"), Origin: "https://example.com/missing.git", Trunk: "main"},
		{Relpath: "sep", Path: sep, Origin: "https://example.com/sep.git", Trunk: "main"},
	}}
	head := func(dir string) string { return strings.TrimSpace(f.RunGit(dir, "rev-parse", "HEAD")) }
	jjParent := strings.TrimSpace(f.RunJJ(jjWS, "log", "--no-graph", "--ignore-working-copy", "-r", "@-", "-T", "commit_id"))
	mainGit := filepath.Join(main, ".git")
	jjGit := filepath.Join(jjMain, ".git")
	want := map[string]discovered{
		jjMain: {Kind: KindJJColocated, Head: head(jjMain), GitDir: jjGit, CommonDir: jjGit},
		jjWS:   {Kind: KindJJWorkspace, Name: "sec\tond\nx", Head: jjParent, CommonDir: jjGit},
		main:   {Kind: KindGit, Branch: "main", Head: head(main), GitDir: mainGit, CommonDir: mainGit},
		feat:   {Kind: KindGit, Name: "feat", Branch: "feat", Head: head(feat), GitDir: filepath.Join(mainGit, "worktrees", "feat"), CommonDir: mainGit},
		locked: {Kind: KindGit, Name: "locked", Head: head(locked), GitDir: filepath.Join(mainGit, "worktrees", "locked"), CommonDir: mainGit, Locked: true},
		sep:    {Kind: KindGit, Branch: "main", Head: head(sep), GitDir: sepGit, CommonDir: sepGit},
	}
	wantSkips := []Skip{
		{Path: filepath.Join(repos, "missing"), Reason: "checkout missing"},
		{Path: gone, Reason: "prunable"},
	}

	opHead := f.JJOpHead(jjMain)
	jjBefore := f.SnapshotTree(filepath.Join(jjMain, ".jj"))
	gitBefore := f.SnapshotTree(mainGit)
	got, skips, err := Discover(context.Background(), reg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if f.JJOpHead(jjMain) != opHead || !reflect.DeepEqual(f.SnapshotTree(filepath.Join(jjMain, ".jj")), jjBefore) || !reflect.DeepEqual(f.SnapshotTree(mainGit), gitBefore) {
		t.Fatal("Discover wrote a source repository")
	}
	gotByRoot := map[string]discovered{}
	ids := map[string]string{}
	for _, wt := range got {
		if !isHex(wt.ID, 32) || wt.ID != worktreeID(wt.Origin, wt.Root, wt.Incarnation) || wt.Incarnation == 0 {
			t.Errorf("%s: ID %q incarnation %d", wt.Root, wt.ID, wt.Incarnation)
		}
		if prev, dup := ids[wt.ID]; dup {
			t.Errorf("duplicate ID %s for %s and %s", wt.ID, prev, wt.Root)
		}
		ids[wt.ID] = wt.Root
		if err := wt.check(); err != nil {
			t.Errorf("%s: %v", wt.Root, err)
		}
		gotByRoot[wt.Root] = discovered{wt.Kind, wt.Name, wt.Branch, wt.Head, wt.GitDir, wt.CommonDir, wt.Locked}
	}
	if !reflect.DeepEqual(gotByRoot, want) {
		t.Errorf("Discover worktrees:\n got %+v\nwant %+v", gotByRoot, want)
	}
	sort.Slice(skips, func(i, j int) bool { return skips[i].Path < skips[j].Path })
	if !reflect.DeepEqual(skips, wantSkips) {
		t.Errorf("Discover skips = %+v, want %+v", skips, wantSkips)
	}

	again, _, err := Discover(context.Background(), reg)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("second Discover differs (err %v)", err)
	}

	featID := idOf(t, got, feat)
	f.RunGit(main, "worktree", "remove", feat)
	f.RunGit(main, "worktree", "add", "-q", feat, "feat")
	recreated, _, err := Discover(context.Background(), reg)
	if err != nil {
		t.Fatalf("Discover after re-add: %v", err)
	}
	if idOf(t, recreated, feat) == featID {
		t.Fatalf("worktree recreated at %s kept ID %s", feat, featID)
	}
	if idOf(t, recreated, main) != idOf(t, got, main) {
		t.Fatal("main worktree ID changed")
	}
}

func idOf(t *testing.T, wts []Worktree, root string) string {
	t.Helper()
	for _, wt := range wts {
		if wt.Root == root {
			return wt.ID
		}
	}
	t.Fatalf("no worktree at %s", root)
	return ""
}

func TestLocate(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(dir, "repo")
	inner := filepath.Join(outer, "nested", "ws")
	sibling := filepath.Join(dir, "repo2")
	for _, d := range []string{filepath.Join(inner, "src"), sibling} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outer, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(inner, link); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(outer, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	wts := []Worktree{{Root: outer, Name: "outer"}, {Root: inner, Name: "inner"}, {Root: sibling, Name: "sibling"}}
	tests := []struct {
		name string
		path string
		want string
	}{
		{"root itself", outer, "outer"},
		{"longest root wins", filepath.Join(inner, "src"), "inner"},
		{"symlinked path resolves", filepath.Join(link, "src"), "inner"},
		{"prefix is not containment", sibling, "sibling"},
		{"deleted subdir of a root", filepath.Join(outer, "gone", "deeper"), "outer"},
		{"deleted subdir through a symlinked ancestor", filepath.Join(alias, "deleted", "file"), "outer"},
		{"deleted subdir through a symlinked root", filepath.Join(link, "gone"), "inner"},
		{"relative path from cwd", filepath.Join("repo", "file"), "outer"},
		{"relative nested path from cwd", filepath.Join("repo", "nested", "ws", "src"), "inner"},
		{"relative deleted path through a symlink", filepath.Join("alias", "gone", "x"), "outer"},
		{"relative path outside every root", "elsewhere", ""},
		{"outside every root", dir, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Locate(wts, tc.path)
			if ok != (tc.want != "") || got.Name != tc.want {
				t.Fatalf("Locate(%s) = (%q, %v), want %q", tc.path, got.Name, ok, tc.want)
			}
		})
	}
}
