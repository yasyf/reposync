package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/registry"
)

type stampKind struct {
	name   string
	kind   Kind
	setup  func(f *vcstest.Fixture, dir string) (main, root string)
	commit func(f *vcstest.Fixture, root string)
}

var stampKinds = []stampKind{
	{
		name:   "git",
		kind:   KindGit,
		setup:  func(f *vcstest.Fixture, dir string) (string, string) { main := f.GitClone(dir); return main, main },
		commit: stampGitCommit,
	},
	{
		name: "linked",
		kind: KindGit,
		setup: func(f *vcstest.Fixture, dir string) (string, string) {
			main := f.GitClone(dir)
			return main, f.LinkedWorktree(main, dir+"-linked", "feat")
		},
		commit: stampGitCommit,
	},
	{
		name:   "jj-colocated",
		kind:   KindJJColocated,
		setup:  func(f *vcstest.Fixture, dir string) (string, string) { main := f.JJClone(dir); return main, main },
		commit: stampJJCommit,
	},
	{
		name: "jj-workspace",
		kind: KindJJWorkspace,
		setup: func(f *vcstest.Fixture, dir string) (string, string) {
			main := f.JJClone(dir)
			return main, f.JJWorkspace(main, dir+"-ws", "second")
		},
		commit: stampJJCommit,
	},
}

func stampGitCommit(f *vcstest.Fixture, root string) {
	f.RunGit(root, "add", "-A")
	f.RunGit(root, "commit", "-q", "--allow-empty", "-m", "wip")
}

func stampJJCommit(f *vcstest.Fixture, root string) {
	f.RunJJ(root, "commit", "-m", "wip")
}

func TestStampChanges(t *testing.T) {
	type step func(t *testing.T, f *vcstest.Fixture, k stampKind, root string)
	write := func(rel, content string) step {
		return func(t *testing.T, _ *vcstest.Fixture, _ stampKind, root string) {
			writeStampFile(t, root, rel, content)
		}
	}
	tests := []struct {
		name       string
		prepare    step
		mutate     step
		changes    bool
		needsIndex bool
	}{
		{name: "unstaged tracked edit", mutate: write("a.txt", "changed\n"), changes: true},
		{name: "new untracked file", mutate: write("new/file.txt", "x\n"), changes: true},
		{
			name: "same-size edit of a dirty file",
			prepare: func(t *testing.T, _ *vcstest.Fixture, _ stampKind, root string) {
				writeStampFile(t, root, "a.txt", "dirty-1\n")
				setStampMtime(t, filepath.Join(root, "a.txt"), time.Unix(1_700_000_000, 0))
			},
			mutate: func(t *testing.T, _ *vcstest.Fixture, _ stampKind, root string) {
				writeStampFile(t, root, "a.txt", "dirty-2\n")
				setStampMtime(t, filepath.Join(root, "a.txt"), time.Unix(1_700_000_001, 0))
			},
			changes: true,
		},
		{
			name:    "staged change",
			prepare: write("a.txt", "staged\n"),
			mutate: func(_ *testing.T, f *vcstest.Fixture, _ stampKind, root string) {
				f.RunGit(root, "add", "a.txt")
			},
			changes:    true,
			needsIndex: true,
		},
		{
			name:    "new commit",
			mutate:  func(_ *testing.T, f *vcstest.Fixture, k stampKind, root string) { k.commit(f, root) },
			changes: true,
		},
		{
			name: "deleted tracked file",
			mutate: func(t *testing.T, _ *vcstest.Fixture, _ stampKind, root string) {
				if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
					t.Fatal(err)
				}
			},
			changes: true,
		},
		{
			name:    "ignored node_modules tree",
			prepare: write("node_modules/pkg/index.js", "one\n"),
			mutate: func(t *testing.T, _ *vcstest.Fixture, _ stampKind, root string) {
				writeStampFile(t, root, "node_modules/pkg/index.js", "two, longer\n")
				writeStampFile(t, root, "node_modules/other/deep/lib.js", "new\n")
			},
		},
		{
			name:    "ignored log files",
			prepare: write("debug.log", "one\n"),
			mutate: func(t *testing.T, _ *vcstest.Fixture, _ stampKind, root string) {
				writeStampFile(t, root, "debug.log", "two, longer\n")
				writeStampFile(t, root, "logs/trace.log", "new\n")
			},
		},
		{
			name: "edit of a committed file later ignored",
			prepare: func(t *testing.T, f *vcstest.Fixture, k stampKind, root string) {
				writeStampFile(t, root, "tracked.txt", "one\n")
				k.commit(f, root)
				writeStampFile(t, root, ".gitignore", "node_modules/\n*.log\ntracked.txt\n")
			},
			mutate:  write("tracked.txt", "two, longer\n"),
			changes: true,
		},
		{
			name:    "new file inside a nested repository",
			prepare: nestedStampRepo,
			mutate:  write("nested/noise.log", "noise\n"),
		},
		{
			name:    "edit inside a nested repository",
			prepare: nestedStampRepo,
			mutate:  write("nested/n.txt", "two, longer\n"),
		},
	}
	for _, k := range stampKinds {
		t.Run(k.name, func(t *testing.T) {
			f := vcstest.New(t)
			seedStampOrigin(f)
			for i, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					if tc.needsIndex && k.kind == KindJJWorkspace {
						t.Skip("a jj workspace has no git index to stage into")
					}
					main, root := k.setup(f, filepath.Join(f.Root, fmt.Sprintf("r%d", i)))
					wt := stampWorktree(t, main, root, k.kind)
					if tc.prepare != nil {
						tc.prepare(t, f, k, root)
					}
					before := mustStamp(t, wt)
					tc.mutate(t, f, k, root)
					after := mustStamp(t, wt)
					if changed := before != after; changed != tc.changes {
						t.Fatalf("stamp changed = %v, want %v (before %s, after %s)", changed, tc.changes, before, after)
					}
				})
			}
		})
	}
}

func TestStampReadOnly(t *testing.T) {
	for _, k := range stampKinds {
		t.Run(k.name, func(t *testing.T) {
			f := vcstest.New(t)
			seedStampOrigin(f)
			main, root := k.setup(f, filepath.Join(f.Root, "repo"))
			wt := stampWorktree(t, main, root, k.kind)
			sentinel := filepath.Join(f.Root, "clean-filter-ran")
			f.RunGit(main, "config", "filter.sentinel.clean", "touch '"+sentinel+"'; cat")
			f.RunGit(main, "config", "filter.sentinel.required", "true")
			f.RunGit(main, "config", "core.fsmonitor", fsmonitorSentinel(t, f, "fsmonitor-ran"))
			future := time.Now().Add(time.Hour)
			setStampMtime(t, filepath.Join(root, "c.dat"), future)
			setStampMtime(t, filepath.Join(root, "a.txt"), future)
			writeStampFile(t, root, "b.txt", "edited\n")
			writeStampFile(t, root, "new.txt", "untracked\n")

			dirs := []string{wt.CommonDir}
			if k.kind != KindGit {
				dirs = append(dirs, filepath.Join(main, ".jj"), filepath.Join(root, ".jj"))
			}
			snapshot := func() []map[string]vcstest.TreeEntry {
				trees := make([]map[string]vcstest.TreeEntry, 0, len(dirs))
				for _, d := range dirs {
					trees = append(trees, f.SnapshotTree(d))
				}
				return trees
			}
			opHead := ""
			if k.kind != KindGit {
				opHead = f.JJOpHead(main)
			}
			before := snapshot()
			mustStamp(t, wt)
			if !reflect.DeepEqual(snapshot(), before) {
				t.Fatal("Stamp wrote the source repository")
			}
			if k.kind != KindGit && f.JJOpHead(main) != opHead {
				t.Fatal("Stamp advanced the jj operation log")
			}
			for _, ran := range []string{"clean-filter-ran", "fsmonitor-ran"} {
				if f.FileExists(f.Root, ran) {
					t.Fatalf("Stamp left %s", ran)
				}
			}
			if k.kind == KindJJWorkspace {
				return
			}
			f.RunGit(root, "status")
			for _, ran := range []string{"clean-filter-ran", "fsmonitor-ran"} {
				if !f.FileExists(f.Root, ran) {
					t.Fatalf("a plain git status never left %s; the fixture proves nothing", ran)
				}
			}
		})
	}
}

func TestStampSubmodule(t *testing.T) {
	f := vcstest.New(t)
	seedStampOrigin(f)
	sub := filepath.Join(f.Root, "subrepo")
	f.RunGit(f.Root, "init", "-q", "-b", "main", sub)
	f.ConfigGit(sub)
	f.WriteFile(sub, ".gitignore", "*.log\n")
	f.WriteFile(sub, ".gitattributes", "*.dat filter=subspy\n")
	f.WriteFile(sub, "s.txt", "sub\n")
	f.WriteFile(sub, "x.dat", "data\n")
	f.RunGit(sub, "add", "-A")
	f.RunGit(sub, "commit", "-q", "-m", "sub")
	newRepo := func(name string) (Worktree, string) {
		root := f.GitClone(filepath.Join(f.Root, name))
		f.RunGit(root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
		f.RunGit(root, "commit", "-q", "-m", "add sub")
		return stampWorktree(t, root, root, KindGit), filepath.Join(root, "sub")
	}

	t.Run("read-only", func(t *testing.T) {
		wt, inner := newRepo("ro")
		f.RunGit(inner, "config", "filter.subspy.clean", "touch '"+filepath.Join(f.Root, "sub-filter-ran")+"'; cat")
		f.RunGit(inner, "config", "filter.subspy.required", "true")
		f.RunGit(inner, "config", "core.fsmonitor", fsmonitorSentinel(t, f, "sub-fsmonitor-ran"))
		setStampMtime(t, filepath.Join(inner, "x.dat"), time.Now().Add(time.Hour))
		mustStamp(t, wt)
		for _, ran := range []string{"sub-filter-ran", "sub-fsmonitor-ran"} {
			if f.FileExists(f.Root, ran) {
				t.Fatalf("Stamp left %s inside a submodule", ran)
			}
		}
		f.RunGit(wt.Root, "status")
		for _, ran := range []string{"sub-filter-ran", "sub-fsmonitor-ran"} {
			if !f.FileExists(f.Root, ran) {
				t.Fatalf("a plain git status never left %s; the fixture proves nothing", ran)
			}
		}
	})

	tests := []struct {
		name    string
		dirty   bool
		mutate  func(t *testing.T, inner string)
		changes bool
	}{
		{
			name:    "edit inside a clean submodule",
			mutate:  func(t *testing.T, inner string) { writeStampFile(t, inner, "s.txt", "edited\n") },
			changes: true,
		},
		{
			name:    "untracked file inside a dirty submodule",
			dirty:   true,
			mutate:  func(t *testing.T, inner string) { writeStampFile(t, inner, "new.txt", "new\n") },
			changes: true,
		},
		{
			name: "commit inside a submodule",
			mutate: func(_ *testing.T, inner string) {
				f.ConfigGit(inner)
				f.RunGit(inner, "commit", "-q", "--allow-empty", "-m", "moved")
			},
			changes: true,
		},
		{
			name:   "ignored file inside a dirty submodule",
			dirty:  true,
			mutate: func(t *testing.T, inner string) { writeStampFile(t, inner, "noise.log", "noise\n") },
		},
		{
			name:   "further edit inside a dirty submodule",
			dirty:  true,
			mutate: func(t *testing.T, inner string) { writeStampFile(t, inner, "s.txt", "edited again, longer\n") },
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wt, inner := newRepo(fmt.Sprintf("s%d", i))
			if tc.dirty {
				writeStampFile(t, inner, "s.txt", "dirty\n")
			}
			before := mustStamp(t, wt)
			tc.mutate(t, inner)
			after := mustStamp(t, wt)
			if changed := before != after; changed != tc.changes {
				t.Fatalf("stamp changed = %v, want %v", changed, tc.changes)
			}
		})
	}
}

func TestStampPrunesIgnoredTree(t *testing.T) {
	for _, k := range []stampKind{stampKinds[0], stampKinds[3]} {
		t.Run(k.name, func(t *testing.T) {
			f := vcstest.New(t)
			seedStampOrigin(f)
			main, root := k.setup(f, filepath.Join(f.Root, "repo"))
			wt := stampWorktree(t, main, root, k.kind)
			for d := range 100 {
				dir := filepath.Join(root, "node_modules", fmt.Sprintf("pkg%03d", d))
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
				for i := range 100 {
					f.WriteFile(dir, fmt.Sprintf("f%03d.js", i), "x\n")
				}
			}
			writeStampFile(t, root, "logs/run.log", "log\n")
			writeStampFile(t, root, "a.txt", "edited\n")
			writeStampFile(t, root, "new.txt", "untracked\n")

			records, err := stampRecords(context.Background(), wt)
			if err != nil {
				t.Fatalf("stampRecords: %v", err)
			}
			for _, r := range records {
				if strings.Contains(r, "node_modules") || strings.Contains(r, ".log") {
					t.Errorf("ignored path in stamp input: %s", r)
				}
			}
			for _, want := range []string{`lstat "a.txt" `, `lstat "new.txt" `} {
				if !slices.ContainsFunc(records, func(r string) bool { return strings.HasPrefix(r, want) }) {
					t.Errorf("stamp input lacks %q:\n%s", want, strings.Join(records, "\n"))
				}
			}
			if len(records) > 16 {
				t.Errorf("stamp input has %d records, want a handful:\n%s", len(records), strings.Join(records, "\n"))
			}
		})
	}
}

func TestStampLstatRecords(t *testing.T) {
	dir := t.TempDir()
	writeStampFile(t, dir, "f", "file\n")
	writeStampFile(t, dir, "d/inner", "nested\n")
	records, err := lstatRecords(dir, []string{"gone", "f", "f/child", "d/", "f"})
	if err != nil {
		t.Fatalf("lstatRecords: %v", err)
	}
	tests := []struct {
		prefix string
		suffix string
	}{
		{`lstat "d" dir`, "dir"},
		{`lstat "f" `, "600"},
		{`lstat "f/child" absent`, "absent"},
		{`lstat "gone" absent`, "absent"},
	}
	if len(records) != len(tests) {
		t.Fatalf("records = %q, want %d", records, len(tests))
	}
	for i, tc := range tests {
		if !strings.HasPrefix(records[i], tc.prefix) || !strings.HasSuffix(records[i], tc.suffix) {
			t.Errorf("record %d = %q, want prefix %q suffix %q", i, records[i], tc.prefix, tc.suffix)
		}
	}
}

func fsmonitorSentinel(t *testing.T, f *vcstest.Fixture, ran string) string {
	t.Helper()
	hook := filepath.Join(f.Root, ran+"-hook")
	script := "#!/bin/sh\ntouch '" + filepath.Join(f.Root, ran) + "'\nexit 1\n"
	//nolint:gosec // G306: the fsmonitor hook must be executable; it lives in a test-controlled temp dir.
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return hook
}

func nestedStampRepo(t *testing.T, f *vcstest.Fixture, _ stampKind, root string) {
	f.RunGit(root, "init", "-q", "nested")
	writeStampFile(t, root, "nested/n.txt", "one\n")
}

func seedStampOrigin(f *vcstest.Fixture) {
	f.WriteFile(f.Seed, ".gitignore", "node_modules/\n*.log\n")
	f.WriteFile(f.Seed, ".gitattributes", "*.dat filter=sentinel\n")
	f.WriteFile(f.Seed, "a.txt", "alpha\n")
	f.WriteFile(f.Seed, "b.txt", "bravo\n")
	f.WriteFile(f.Seed, "c.dat", "data\n")
	f.RunGit(f.Seed, "add", "-A")
	f.RunGit(f.Seed, "commit", "-q", "-m", "seed stamp fixture")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
}

func stampWorktree(t *testing.T, main, root string, kind Kind) Worktree {
	t.Helper()
	reg := registry.Registry{Repos: []registry.Repo{{Relpath: "r", Path: main, Origin: "https://example.com/r.git", Trunk: "main"}}}
	wts, _, err := Discover(context.Background(), reg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	wt, ok := Locate(wts, root)
	if !ok || wt.Root != root || wt.Kind != kind {
		t.Fatalf("no %s worktree at %s in %+v", kind, root, wts)
	}
	return wt
}

func mustStamp(t *testing.T, wt Worktree) string {
	t.Helper()
	first, err := Stamp(context.Background(), wt)
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	second, err := Stamp(context.Background(), wt)
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if first != second || !isHex(first, 64) {
		t.Fatalf("repeated Stamp = %q then %q, want one stable sha256", first, second)
	}
	return first
}

func writeStampFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setStampMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}
