package worktree_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
)

func (h *harness) restore(snap worktree.Snapshot, opts worktree.RestoreOptions) worktree.Restored {
	h.t.Helper()
	r, err := h.store.Restore(h.t.Context(), h.recvReg(), snap, h.art, opts)
	if err != nil {
		h.t.Fatalf("restore: %v", err)
	}
	return r
}

func (h *harness) files(dir string) map[string]vcstest.TreeEntry {
	h.t.Helper()
	tree := h.f.SnapshotTree(dir)
	maps.DeleteFunc(tree, func(p string, e vcstest.TreeEntry) bool {
		return e.Mode.IsDir() || p == ".git" || strings.HasPrefix(p, ".git/")
	})
	for p, e := range tree {
		e.Mode = e.Mode.Type() | e.Mode&0o100
		tree[p] = e
	}
	return tree
}

func (h *harness) status(dir string) []string {
	h.t.Helper()
	var recs []string
	for rec := range strings.SplitSeq(h.f.RunGit(dir, "status", "--porcelain=v2", "-z", "--untracked-files=all"), "\x00") {
		if rec != "" && !strings.HasPrefix(rec, "#") {
			recs = append(recs, rec)
		}
	}
	slices.Sort(recs)
	return recs
}

func (h *harness) assertFaithful(r worktree.Restored) {
	h.t.Helper()
	if !r.Exact || len(r.Differences) != 0 || len(r.LFSPending) != 0 {
		h.t.Fatalf("restore not exact: %+v", r)
	}
	if src, dst := h.files(h.src), h.files(r.Path); !maps.Equal(src, dst) {
		h.t.Fatalf("worktree bytes differ:\nsource %v\nrestored %v", src, dst)
	}
	if src, dst := h.status(h.src), h.status(r.Path); !slices.Equal(src, dst) {
		h.t.Fatalf("status differs:\nsource %q\nrestored %q", src, dst)
	}
	if src, dst := h.git(h.src, "log", "--format=%H"), h.git(r.Path, "log", "--format=%H"); src != dst {
		h.t.Fatalf("history differs:\nsource %s\nrestored %s", src, dst)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	h := newGitHarness(t)
	if err := os.Mkdir(filepath.Join(h.src, "becomes-file"), 0o750); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{
		"a.txt": "a1\n", "bin.dat": "\x00\x01bin1", "del-staged.txt": "x\n", "del-unstaged.txt": "y\n",
		"run.sh": "#!/bin/sh\n", ".gitignore": "*.log\n", "becomes-file/inner.txt": "dir\n",
	} {
		h.f.WriteFile(h.src, p, c)
	}
	if err := os.Symlink("a.txt", filepath.Join(h.src, "link")); err != nil {
		t.Fatal(err)
	}
	h.f.RunGit(h.src, "add", "-A")
	h.f.RunGit(h.src, "commit", "-qm", "unpushed base")
	h.commit(h.src, "second.txt", "second unpushed commit\n")

	h.f.WriteFile(h.src, "a.txt", "a2 staged\n")
	h.f.WriteFile(h.src, "bin.dat", "\x00\x02bin2 staged")
	h.f.RunGit(h.src, "add", "a.txt", "bin.dat")
	h.f.WriteFile(h.src, "a.txt", "a3 worktree\n")
	h.f.WriteFile(h.src, "bin.dat", "\x00\x03bin3 worktree")
	h.f.RunGit(h.src, "rm", "-q", "del-staged.txt")
	for _, step := range []func() error{
		func() error { return os.Remove(filepath.Join(h.src, "del-unstaged.txt")) },
		func() error { return os.RemoveAll(filepath.Join(h.src, "becomes-file")) },
		func() error { return os.WriteFile(filepath.Join(h.src, "becomes-file"), []byte("now a file\n"), 0o600) },
		//nolint:gosec // G302: the fixture needs an executable file to prove the exec bit round-trips.
		func() error { return os.Chmod(filepath.Join(h.src, "run.sh"), 0o755) },
		func() error { return os.Remove(filepath.Join(h.src, "link")) },
		func() error { return os.Symlink("bin.dat", filepath.Join(h.src, "link")) },
		func() error { return os.Symlink("a.txt", filepath.Join(h.src, "untracked-link")) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	h.f.WriteFile(h.src, "ita.txt", "intent\n")
	h.f.RunGit(h.src, "add", "-N", "ita.txt")
	if err := os.MkdirAll(filepath.Join(h.src, "dir", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	h.f.WriteFile(h.src, "dir/sub/new.txt", "nested\n")
	h.f.WriteFile(h.src, "dir/ünï cöde\ttab.txt", "unicode\n")
	h.f.WriteFile(h.src, "debug.log", "ignored\n")

	snap := h.seal(h.capture())
	if snap.Head.Ahead != 2 || len(snap.IntentToAdd) != 1 {
		t.Fatalf("fixture snapshot: ahead %d, i-t-a %v", snap.Head.Ahead, snap.IntentToAdd)
	}
	for _, f := range snap.Files {
		if f.Path == "debug.log" {
			t.Fatal("ignored file captured")
		}
	}
	dest := filepath.Join(h.f.Root, "recovered")
	r := h.restore(snap, worktree.RestoreOptions{Dest: dest})
	if r.Path != dest || r.Reused || r.Head != snap.Head.Commit || r.Applied != snap.Digest || !strings.HasPrefix(r.Branch, "recovery/main-hostA-") {
		t.Fatalf("restored = %+v", r)
	}
	if err := os.Remove(filepath.Join(h.src, "debug.log")); err != nil {
		t.Fatal(err)
	}
	h.assertFaithful(r)
	if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
		t.Fatalf("recovery refs left in checkout: %v", refs)
	}
}

func TestRestoreSafety(t *testing.T) {
	h := newGitHarness(t)
	h.f.RunGit(h.recv, "branch", "recovery/taken")
	sentinel := filepath.Join(h.f.Root, "hook-ran")
	for _, hook := range []string{"post-checkout", "reference-transaction", "post-index-change"} {
		script := "#!/bin/sh\necho " + hook + " >> " + sentinel + "\n"
		//nolint:gosec // G306: git runs hooks only when they are executable.
		if err := os.WriteFile(filepath.Join(h.recv, ".git", "hooks", hook), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())

	occupied := filepath.Join(h.f.Root, "occupied")
	if err := os.Mkdir(occupied, 0o750); err != nil {
		t.Fatal(err)
	}
	h.f.WriteFile(occupied, "keep.txt", "mine\n")
	before := h.f.SnapshotTree(occupied)
	if _, err := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: occupied}); !errors.Is(err, worktree.ErrDestinationExists) {
		t.Fatalf("existing dest error = %v, want ErrDestinationExists", err)
	}
	if after := h.f.SnapshotTree(occupied); !maps.Equal(before, after) {
		t.Fatalf("existing dest changed: %v -> %v", before, after)
	}

	r := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "fresh"), Branch: "recovery/taken"})
	if h.f.FileExists(h.f.Root, "hook-ran") {
		t.Fatalf("receiver hooks ran: %q", h.f.ReadFile(h.f.Root, "hook-ran"))
	}
	if r.Branch != "recovery/taken-2" || h.git(r.Path, "branch", "--show-current") != "recovery/taken-2" {
		t.Fatalf("branch = %q, want recovery/taken-2", r.Branch)
	}
	h.assertFaithful(r)
}

func TestRestoreReusesSiblingWithoutRollback(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "v1\n")
	first := h.seal(h.capture())
	one := filepath.Join(h.f.Root, "one")
	r1 := h.restore(first, worktree.RestoreOptions{Dest: one})
	h.assertFaithful(r1)
	h.f.WriteFile(one, "wip.txt", "edited on the receiver\n")

	h.f.WriteFile(h.src, "wip.txt", "v2\n")
	second := h.seal(h.capture())
	two := filepath.Join(h.f.Root, "two")
	r2 := h.restore(second, worktree.RestoreOptions{Dest: two})
	if !r2.Reused || !r2.Newer || r2.Path != one || r2.Applied != first.Digest || r2.Branch != r1.Branch {
		t.Fatalf("sibling restore = %+v, want reuse of %s", r2, one)
	}
	if got := h.f.ReadFile(one, "wip.txt"); got != "edited on the receiver\n" {
		t.Fatalf("reused checkout rolled back: %q", got)
	}
	if _, err := os.Lstat(two); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reuse created %s: %v", two, err)
	}

	r3 := h.restore(second, worktree.RestoreOptions{Dest: two, Fresh: true})
	if r3.Reused || r3.Path != two || r3.Branch == r1.Branch {
		t.Fatalf("fresh restore = %+v", r3)
	}
	h.assertFaithful(r3)
}

func TestRestorePathCollision(t *testing.T) {
	h := newGitHarness(t)
	caseFold, _, err := worktree.Folding(filepath.Join(h.f.Root, "dest"))
	if err != nil {
		t.Fatal(err)
	}
	if !caseFold {
		t.Skip("temp dir is on a case-sensitive filesystem")
	}
	snap := h.capture()
	for _, name := range []string{"A.txt", "a.txt"} {
		snap.Files = append(snap.Files, worktree.FileEntry{Path: name, Kind: worktree.FileRegular, Content: h.put(worktree.MediaFile, []byte(name)), Untracked: true})
	}
	slices.SortFunc(snap.Files, func(a, b worktree.FileEntry) int { return strings.Compare(a.Path, b.Path) })
	snap = h.seal(snap)
	dest := filepath.Join(h.f.Root, "dest")
	if _, err := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: dest}); !errors.Is(err, worktree.ErrPathCollision) {
		t.Fatalf("restore error = %v, want ErrPathCollision", err)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision created %s: %v", dest, err)
	}
}

func TestRestoreLFSWithoutNetwork(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	f.WriteFile(f.Seed, "base.bin", "published base asset")
	f.WriteFile(f.Seed, "other.bin", "another published asset")
	f.RunGit(f.Seed, "add", "base.bin", "other.bin")
	f.RunGit(f.Seed, "commit", "-qm", "base assets")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	h := newHarness(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	lfsURL := h.git(h.recv, "config", "-f", ".lfsconfig", "lfs.url")
	h.f.RunGit(h.recv, "config", "lfs.url", "file://"+filepath.Join(f.Root, "no-such-remote"))

	h.commit(h.src, "unpushed.bin", "object only in an unpushed commit")
	h.f.WriteFile(h.src, "staged.bin", "object only in the index")
	h.f.RunGit(h.src, "add", "staged.bin")
	h.f.WriteFile(h.src, "base.bin", "raw edit of a tracked lfs file")
	snap := h.capture()
	common := filepath.Join(h.src, ".git")
	for _, p := range []string{"staged.bin", "unpushed.bin"} {
		data := []byte(h.f.ReadFile(h.src, p))
		oid := vcstest.SHA256(data)
		stored, err := os.ReadFile(vcstest.LFSObjectPath(common, oid))
		if err != nil {
			t.Fatalf("source lfs object for %s: %v", p, err)
		}
		snap.LFSObjects = append(snap.LFSObjects, worktree.LFSObject{OID: oid, Size: int64(len(data)), Artifact: *h.put(worktree.MediaLFSObject, stored)})
		if snap.LFS == nil {
			snap.LFS = &worktree.LFSInfo{Remote: lfsURL}
		}
		snap.LFS.Objects = append(snap.LFS.Objects, worktree.LFSObjectRef{Path: p, OID: oid, Size: int64(len(data))})
	}
	slices.SortFunc(snap.LFSObjects, func(a, b worktree.LFSObject) int { return strings.Compare(a.OID, b.OID) })
	snap = h.seal(snap)

	r := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "lfs-restore")})
	h.assertFaithful(r)
	for _, p := range []string{"unpushed.bin", "staged.bin", "base.bin", "other.bin"} {
		if got, want := h.f.ReadFile(r.Path, p), h.f.ReadFile(h.src, p); got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}

	otherOID := vcstest.SHA256([]byte("another published asset"))
	if err := os.Remove(vcstest.LFSObjectPath(filepath.Join(h.recv, ".git"), otherOID)); err != nil {
		t.Fatal(err)
	}
	pending := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "lfs-pending"), Fresh: true})
	if pending.Exact || !slices.Equal(pending.LFSPending, []string{"other.bin"}) || !slices.Equal(pending.Differences, []string{"other.bin: lfs object pending"}) {
		t.Fatalf("pending restore = %+v", pending)
	}
	if got := h.f.ReadFile(pending.Path, "other.bin"); !strings.HasPrefix(got, "version https://git-lfs.github.com/spec/v1") {
		t.Fatalf("other.bin = %q, want an unhydrated pointer", got)
	}

	h.f.RunGit(h.recv, "config", "lfs.url", lfsURL)
	fetched := h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "lfs-fetched"), Fresh: true, FetchLFS: true})
	h.assertFaithful(fetched)
}

func TestRestoreSHA256(t *testing.T) {
	f := vcstest.New(t)
	origin := filepath.Join(f.Root, "origin256.git")
	seed := filepath.Join(f.Root, "seed256")
	f.RunGit(f.Root, "init", "-q", "--bare", "--object-format=sha256", "-b", "main", origin)
	f.RunGit(f.Root, "clone", "-q", origin, seed)
	f.ConfigGit(seed)
	f.WriteFile(seed, "README.md", "sha256\n")
	f.RunGit(seed, "add", "README.md")
	f.RunGit(seed, "commit", "-qm", "init")
	f.RunGit(seed, "push", "-q", "origin", "main")
	clone := func(name string) string {
		dest := filepath.Join(f.Root, name)
		f.RunGit(f.Root, "clone", "-q", origin, dest)
		f.ConfigGit(dest)
		return dest
	}
	h := newHarness(t, f, clone("src"), clone("recv"))
	h.commit(h.src, "feature.txt", "unpushed\n")
	h.f.WriteFile(h.src, "README.md", "staged\n")
	h.f.RunGit(h.src, "add", "README.md")
	h.f.WriteFile(h.src, "README.md", "worktree\n")
	snap := h.seal(h.capture())
	if snap.ObjectFormat != "sha256" || len(snap.Head.Commit) != 64 {
		t.Fatalf("fixture is not sha256: %s %s", snap.ObjectFormat, snap.Head.Commit)
	}
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered")}))
}
