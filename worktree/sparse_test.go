package worktree_test

import (
	"path/filepath"
	"reflect"
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
