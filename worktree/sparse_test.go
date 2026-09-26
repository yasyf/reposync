package worktree_test

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
)

func sparseSource(t *testing.T, f *vcstest.Fixture, linked bool, args ...string) string {
	t.Helper()
	src := f.GitClone(filepath.Join(f.Root, "src"))
	writeBytes(t, src, "kept/inside.txt", []byte("inside\n"))
	writeBytes(t, src, "excluded/outside.txt", []byte("outside\n"))
	f.RunGit(src, "add", "kept", "excluded")
	f.RunGit(src, "commit", "-qm", "layout")
	if linked {
		src = f.LinkedWorktree(src, filepath.Join(f.Root, "linked"), "feature")
	}
	f.RunGit(src, append([]string{"sparse-checkout", "set"}, args...)...)
	if f.FileExists(src, "excluded/outside.txt") {
		t.Fatal("sparse-checkout left excluded/outside.txt")
	}
	return src
}

func TestCaptureSparse(t *testing.T) {
	cone := &worktree.Sparse{Cone: true, Patterns: []string{"/*", "!/*/", "/kept/"}}
	tests := []struct {
		name   string
		linked bool
		args   []string
		config [][2]string
		want   *worktree.Sparse
	}{
		{"main-cone", false, []string{"kept"}, nil, cone},
		{"linked-cone", true, []string{"kept"}, nil, cone},
		{"main-no-cone", false, []string{"--no-cone", "/kept/"}, nil, &worktree.Sparse{Patterns: []string{"/kept/"}}},
		{"mixed-case-booleans", false, []string{"kept"}, [][2]string{{"core.sparseCheckout", "On"}, {"core.sparseCheckoutCone", "YES"}}, cone},
		{"cone-disabled", true, []string{"kept"}, [][2]string{{"core.SparseCheckoutCone", "False"}}, &worktree.Sparse{Patterns: cone.Patterns}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			src := sparseSource(t, f, tt.linked, tt.args...)
			for _, kv := range tt.config {
				f.RunGit(src, "config", "--worktree", kv[0], kv[1])
			}
			snap := mustCapture(t, openStore(t), discoverAt(t, src, src), newHarness(t, f, src, src).art)
			if !reflect.DeepEqual(snap.Sparse, tt.want) {
				t.Fatalf("Sparse = %+v, want %+v", snap.Sparse, tt.want)
			}
		})
	}
}

func TestRestoreSparse(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		apply bool
		list  string
	}{
		{"default-expands-and-says-so", []string{"kept"}, false, ""},
		{"apply-cone", []string{"kept"}, true, "kept"},
		{"apply-no-cone", []string{"--no-cone", "/kept/"}, true, "/kept/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			src := sparseSource(t, f, false, tt.args...)
			rt := newRoundTrip(t, f, src, f.GitClone(filepath.Join(f.Root, "recv")))
			rt.f.WriteFile(rt.src, "kept/inside.txt", "edited inside\n")
			snap, want := rt.tick()
			if snap.Sparse == nil {
				t.Fatal("capture dropped the sparse-checkout configuration")
			}
			r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered"), ApplySparse: tt.apply})
			if !reflect.DeepEqual(r.Sparse, snap.Sparse) {
				t.Fatalf("Restored.Sparse = %+v, want %+v", r.Sparse, snap.Sparse)
			}
			got := rt.worktreeFiles(r.Path)
			if !tt.apply {
				expansion := slices.ContainsFunc(r.Differences, func(d string) bool {
					return strings.Contains(d, "sparse checkout expanded to full") && strings.Contains(d, "/kept/")
				})
				if r.Exact || !expansion || !f.FileExists(r.Path, "excluded/outside.txt") {
					t.Fatalf("restored %+v, want Exact=false naming the full expansion of %v", r, snap.Sparse.Patterns)
				}
				return
			}
			if !r.Exact || len(r.Differences) != 0 || !maps.Equal(want.files, got) {
				t.Fatalf("restored %+v files differ at %q", r, changed(want.files, got))
			}
			if status := rt.status(r.Path); !slices.Equal(want.status, status) {
				t.Fatalf("status differs:\nsource   %q\nrestored %q", want.status, status)
			}
			if list := rt.git(r.Path, "sparse-checkout", "list"); list != tt.list {
				t.Fatalf("sparse-checkout list = %q, want %q", list, tt.list)
			}
		})
	}
}

func TestSparseRoundTripFidelity(t *testing.T) {
	tests := []struct {
		name       string
		apply      bool
		source     func(t *testing.T, f *vcstest.Fixture, src string)
		receiver   func(t *testing.T, f *vcstest.Fixture, recv string)
		patterns   []string
		exceptions []string
	}{
		{"leading-space-pattern", true, func(_ *testing.T, f *vcstest.Fixture, src string) {
			f.RunGit(src, "sparse-checkout", "set", "--no-cone", " kept/")
		}, nil, []string{" kept/"}, nil},
		{"restored-outside", true, func(_ *testing.T, f *vcstest.Fixture, src string) {
			f.RunGit(src, "sparse-checkout", "set", "kept")
			f.RunGit(src, "restore", "--ignore-skip-worktree-bits", "--", "excluded/outside.txt")
		}, nil, []string{"/*", "!/*/", "/kept/"}, []string{"excluded/outside.txt"}},
		{"staged-outside", true, func(t *testing.T, f *vcstest.Fixture, src string) {
			f.RunGit(src, "sparse-checkout", "set", "kept")
			writeBytes(t, src, "excluded/outside.txt", []byte("staged outside\n"))
			f.RunGit(src, "add", "--sparse", "excluded/outside.txt")
		}, nil, []string{"/*", "!/*/", "/kept/"}, []string{"excluded/outside.txt"}},
		{"hidden-inside", true, func(t *testing.T, f *vcstest.Fixture, src string) {
			f.RunGit(src, "sparse-checkout", "set", "kept")
			f.RunGit(src, "update-index", "--skip-worktree", "kept/inside.txt")
			if err := os.Remove(filepath.Join(src, "kept", "inside.txt")); err != nil {
				t.Fatal(err)
			}
		}, nil, []string{"/*", "!/*/", "/kept/"}, []string{"kept/inside.txt"}},
		{"sparse-receiver-full-source", false, func(_ *testing.T, f *vcstest.Fixture, src string) {
			f.WriteFile(src, "kept/inside.txt", "edited inside\n")
		}, func(_ *testing.T, f *vcstest.Fixture, recv string) {
			f.RunGit(recv, "sparse-checkout", "set", "kept")
		}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			writeBytes(t, f.Seed, " kept/space.txt", []byte("space\n"))
			writeBytes(t, f.Seed, "kept/inside.txt", []byte("inside\n"))
			writeBytes(t, f.Seed, "excluded/outside.txt", []byte("outside\n"))
			f.RunGit(f.Seed, "add", "-A")
			f.RunGit(f.Seed, "commit", "-qm", "layout")
			f.RunGit(f.Seed, "push", "-q", "origin", "main")
			rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
			tt.source(t, f, rt.src)
			if tt.receiver != nil {
				tt.receiver(t, f, rt.recv)
			}
			snap, want := rt.tick()
			var patterns, exceptions []string
			if snap.Sparse != nil {
				patterns, exceptions = snap.Sparse.Patterns, snap.Sparse.Exceptions
			}
			if !slices.Equal(patterns, tt.patterns) || !slices.Equal(exceptions, tt.exceptions) {
				t.Fatalf("captured sparse %+v, want patterns %q exceptions %q", snap.Sparse, tt.patterns, tt.exceptions)
			}
			r := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered"), ApplySparse: tt.apply})
			if got := rt.worktreeFiles(r.Path); !r.Exact || !maps.Equal(want.files, got) {
				t.Fatalf("restored %+v files differ at %q", r, changed(want.files, got))
			}
			if status := rt.status(r.Path); !slices.Equal(want.status, status) {
				t.Fatalf("status differs:\nsource   %q\nrestored %q", want.status, status)
			}
			if src, got := rt.git(rt.src, "ls-files", "-t"), rt.git(r.Path, "ls-files", "-t"); src != got {
				t.Fatalf("skip-worktree bits differ:\nsource\n%s\nrestored\n%s", src, got)
			}
		})
	}
}
