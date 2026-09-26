package worktree

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

func TestReadStatusRealRepo(t *testing.T) {
	f := vcstest.New(t)
	repo := f.GitClone(filepath.Join(f.Root, "repo"))
	f.WriteFile(repo, "del.txt", "del\n")
	f.WriteFile(repo, "exec.sh", "#!/bin/sh\n")
	f.RunGit(repo, "add", ".")
	f.RunGit(repo, "commit", "-qm", "files")
	f.WriteFile(repo, "README.md", "staged\n")
	f.RunGit(repo, "add", "README.md")
	f.WriteFile(repo, "README.md", "worktree\n")
	f.WriteFile(repo, "staged.txt", "new\n")
	f.RunGit(repo, "add", "staged.txt")
	f.WriteFile(repo, "ita.txt", "ita\n")
	f.RunGit(repo, "add", "-N", "ita.txt")
	if err := os.Remove(filepath.Join(repo, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "exec.sh"), 0o700); err != nil { //nolint:gosec // G302: the test needs an executable-bit change.
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.WriteFile(repo, "dir/sp ace\nnl.txt", "u\n")
	f.RunGit(repo, "init", "-q", filepath.Join(repo, "nested"))

	got, err := readStatus(context.Background(), repo, vcs.ReadOnlyGitEnv())
	if err != nil {
		t.Fatalf("readStatus: %v", err)
	}
	rev := func(spec string) string { return strings.TrimSpace(f.RunGit(repo, "rev-parse", spec)) }
	zero := strings.Repeat("0", 40)
	want := statusReport{
		commit:   rev("HEAD"),
		branch:   "main",
		upstream: "origin/main",
		changed: []statusEntry{
			{'M', 'M', "N...", "100644", "100644", "100644", rev("HEAD:README.md"), rev(":README.md"), "README.md"},
			{'.', 'D', "N...", "100644", "100644", "000000", rev("HEAD:del.txt"), rev(":del.txt"), "del.txt"},
			{'.', 'M', "N...", "100644", "100644", "100755", rev("HEAD:exec.sh"), rev(":exec.sh"), "exec.sh"},
			{'.', 'A', "N...", "000000", "000000", "100644", zero, zero, "ita.txt"},
			{'A', '.', "N...", "000000", "100644", "100644", zero, rev(":staged.txt"), "staged.txt"},
		},
		untracked: []string{"dir/sp ace\nnl.txt", "nested/"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readStatus:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    statusReport
		wantErr string
	}{
		{
			name: "unborn detached headers",
			in:   "# branch.oid (initial)\x00# branch.head (detached)\x00",
			want: statusReport{},
		},
		{
			name: "unmerged and dirty submodule",
			in:   "u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict file.txt\x001 .M SCM. 160000 160000 160000 ddd ddd sub\x00",
			want: statusReport{
				unmerged: []string{"conflict file.txt"},
				changed:  []statusEntry{{'.', 'M', "SCM.", "160000", "160000", "160000", "ddd", "ddd", "sub"}},
			},
		},
		{name: "rename record", in: "2 R. N... 100644 100644 100644 a a R100 new\x00old\x00", wantErr: "unexpected status record"},
		{name: "ignored record", in: "! build/\x00", wantErr: "unexpected status record"},
		{name: "short ordinary record", in: "1 .M N... 100644\x00", wantErr: "malformed status record"},
		{name: "not NUL-terminated", in: "? a", wantErr: "not NUL-terminated"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatus([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseStatus error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStatus: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseStatus = %+v, want %+v", got, tc.want)
			}
		})
	}
	if (statusEntry{sub: "N..."}).submoduleChanged() || !(statusEntry{sub: "S..U"}).submoduleChanged() || (statusEntry{sub: "S..."}).submoduleChanged() {
		t.Fatal("submoduleChanged misclassifies the sub field")
	}
}
