package worktree_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
	"github.com/yasyf/reposync/worktree/worktreetest"
)

func allowFetch(ctx context.Context, fetch func(context.Context) error) error { return fetch(ctx) }

func deferFetch(context.Context, func(context.Context) error) error { return worktree.ErrFetchDeferred }

func countGate(calls *int, gate worktree.FetchGate) worktree.FetchGate {
	return func(ctx context.Context, fetch func(context.Context) error) error {
		*calls++
		return gate(ctx, fetch)
	}
}

func snapshotRef(snap worktree.Snapshot, name string) string {
	return worktree.SnapshotPrefix + worktree.SnapshotKey(snap) + "/" + name
}

func (h *harness) verifyFrom(src worktree.ArtifactSource, snap worktree.Snapshot, opts worktree.VerifyOptions) worktree.Verification {
	h.t.Helper()
	v, err := h.store.Verify(h.t.Context(), h.recvReg(), snap, src, opts)
	if err != nil {
		h.t.Fatalf("verify: %v", err)
	}
	return v
}

func (h *harness) release(snap worktree.Snapshot) {
	h.t.Helper()
	if err := h.store.Release(h.t.Context(), h.recvReg(), snap); err != nil {
		h.t.Fatalf("release: %v", err)
	}
}

func (h *harness) gc() {
	h.t.Helper()
	h.git(h.recv, "reflog", "expire", "--expire=now", "--all")
	h.git(h.recv, "gc", "-q", "--prune=now")
	h.git(h.f.Root, "--git-dir="+h.mirrorDir(), "gc", "-q", "--prune=now")
}

func (rt *roundTrip) ship(snap worktree.Snapshot, held ...worktree.ArtifactRef) (worktree.Snapshot, *worktreetest.Store) {
	rt.t.Helper()
	enc, err := worktree.Encode(snap)
	if err != nil {
		rt.t.Fatal(err)
	}
	manifest, err := rt.art.Put(rt.t.Context(), worktree.MediaSnapshot, bytes.NewReader(enc))
	if err != nil {
		rt.t.Fatal(err)
	}
	refs := slices.DeleteFunc(append(snap.Artifacts(), manifest), func(r worktree.ArtifactRef) bool { return slices.Contains(held, r) })
	onRecv := worktreetest.New()
	if err := rt.art.CopyTo(onRecv, refs); err != nil {
		rt.t.Fatal(err)
	}
	received, err := worktree.Decode([]byte(readArtifact(rt.t, onRecv, manifest)))
	if err != nil {
		rt.t.Fatal(err)
	}
	return received, onRecv
}

func TestLifecycleCyclesOnReceiver(t *testing.T) {
	f := vcstest.New(t)
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
	for i, name := range []string{"one", "two"} {
		rt.commit(rt.src, name+".txt", "unpushed "+name+"\n")
		f.WriteFile(rt.src, "README.md", "wip "+name+"\n")
		f.WriteFile(rt.src, "staged-"+name+".txt", "staged "+name+"\n")
		f.RunGit(rt.src, "add", "staged-"+name+".txt")
		snap, want := rt.tick()
		if !snap.Complete || len(snap.History) != i+1 {
			t.Fatalf("cycle %s: complete %t history %+v, want %d links", name, snap.Complete, snap.History, i+1)
		}
		received, onRecv := rt.ship(snap)
		r := rt.pickup(rt.store, rt.recv, onRecv, received, worktree.RestoreOptions{Dest: filepath.Join(f.Root, name), Fresh: i > 0})
		rt.assertRestored(received, r, want)

		rt.release(received)
		if pins, refs := rt.pins(), rt.mirrorRefs(); len(pins) != 0 || len(refs) != 0 {
			t.Fatalf("cycle %s: after release pins %v mirror refs %v, want none", name, pins, refs)
		}
		rt.gc()
		rt.git(rt.recv, "fsck", "--connectivity-only", "--no-dangling")
		if got := rt.worktreeFiles(r.Path); !maps.Equal(want.files, got) {
			t.Fatalf("cycle %s: restored worktree changed by release and gc at %q", name, changed(want.files, got))
		}
		if got := rt.git(r.Path, "log", "--format=%H"); got != want.log {
			t.Fatalf("cycle %s: restored history after gc:\nwant %s\ngot  %s", name, want.log, got)
		}

		if v := rt.verifyFrom(onRecv, received, worktree.VerifyOptions{}); !v.Ready || len(v.Missing) != 0 {
			t.Fatalf("cycle %s: re-verify after release and gc = %+v, want ready", name, v)
		}
		if refs := rt.mirrorRefs(); refs[snapshotRef(received, "head")] != received.Head.Commit || refs[worktree.TipPrefix+received.Head.Commit] != received.Head.Commit {
			t.Fatalf("cycle %s: mirror refs after re-verify = %v", name, refs)
		}
		rt.release(received)
		if pins, refs := rt.pins(), rt.mirrorRefs(); len(pins) != 0 || len(refs) != 0 {
			t.Fatalf("cycle %s: after second release pins %v mirror refs %v, want none", name, pins, refs)
		}
	}
}

func TestLifecycleChainRebuiltAfterTrunkForcePush(t *testing.T) {
	tests := []struct {
		name   string
		rebase bool
	}{
		{"rebased onto the rewritten trunk", true},
		{"left on the dropped trunk commit", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.New(t)
			base := f.OriginMain()
			dropped := f.AdvanceOrigin("published, then force-pushed away")
			rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
			rt.commit(rt.src, "feature.txt", "feature\n")
			f.WriteFile(rt.src, "notes.txt", "wip\n")
			old, oldWant := rt.tick()
			if len(old.History) != 1 || !slices.Equal(old.Requires, []string{dropped}) {
				t.Fatalf("old history %+v requires %v, want one link on [%s]", old.History, old.Requires, dropped)
			}
			first := rt.pickup(rt.store, rt.recv, rt.art, old, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "old")})
			rt.assertRestored(old, first, oldWant)

			f.RunGit(f.Seed, "reset", "-q", "--hard", base)
			f.WriteFile(f.Seed, "rewritten.txt", "rewritten trunk\n")
			f.RunGit(f.Seed, "add", "rewritten.txt")
			f.RunGit(f.Seed, "commit", "-qm", "rewritten trunk")
			f.RunGit(f.Seed, "push", "-q", "--force", "origin", "main")
			rewritten := f.OriginMain()
			f.RunGit(rt.src, "fetch", "-q", "origin")
			requires, pins, fetches := []string{base}, slices.Sorted(slices.Values([]string{base, dropped})), 0
			if tt.rebase {
				f.RunGit(rt.src, "rebase", "-q", "--onto", "origin/main", dropped)
				requires, pins, fetches = []string{rewritten}, []string{rewritten}, 1
			}
			snap, want := rt.tick()
			if len(snap.History) != 1 || snap.History[0].Artifact == old.History[0].Artifact || !slices.Equal(snap.Requires, requires) {
				t.Fatalf("rebuilt history %+v requires %v, want one fresh link on %v", snap.History, snap.Requires, requires)
			}

			calls := 0
			if v := rt.verifyFrom(rt.art, snap, worktree.VerifyOptions{FetchOrigin: countGate(&calls, allowFetch)}); !v.Ready || calls != fetches {
				t.Fatalf("verify rebuilt chain = %+v with %d origin fetches, want ready after %d", v, calls, fetches)
			}
			second := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "new"), Fresh: true})
			rt.assertRestored(snap, second, want)

			rt.release(old)
			if got := rt.pins(); !slices.Equal(got, pins) {
				t.Fatalf("pins after releasing the old snapshot = %v, want %v", got, pins)
			}
			refs := rt.mirrorRefs()
			if _, ok := refs[snapshotRef(old, "head")]; ok || refs[snapshotRef(snap, "index")] == "" || refs[worktree.TipPrefix+snap.History[0].Tip] != snap.History[0].Tip {
				t.Fatalf("mirror refs after releasing the old snapshot = %v", refs)
			}
			if _, ok := refs[worktree.TipPrefix+old.History[0].Tip]; tt.rebase && ok {
				t.Fatalf("released tip %s still mirrored: %v", old.History[0].Tip, refs)
			}

			f.RunGit(rt.recv, "fetch", "-q", "origin")
			f.RunGit(rt.recv, "reset", "-q", "--hard", "origin/main")
			f.RunGit(rt.recv, "update-ref", "-d", "ORIG_HEAD")
			for _, r := range []worktree.Restored{first, second} {
				f.RunGit(rt.recv, "worktree", "remove", "--force", r.Path)
				f.RunGit(rt.recv, "branch", "-q", "-D", r.Branch)
			}
			rt.gc()

			received, onRecv := rt.ship(snap, snap.History[0].Artifact)
			rt.assertRestored(snap, rt.pickup(rt.store, rt.recv, onRecv, received, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "after-gc")}), want)
			if got := rt.pins(); !slices.Equal(got, pins) {
				t.Fatalf("pins after gc = %v, want %v", got, pins)
			}
		})
	}
}

func TestLifecycleSuccessiveSnapshotsReuseMirroredTips(t *testing.T) {
	f := vcstest.New(t)
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
	trunk := f.OriginMain()
	tipA := rt.commit(rt.src, "a.txt", "first unpushed commit\n")
	f.WriteFile(rt.src, "README.md", "wip one\n")
	first, wantFirst := rt.tick()
	f.WriteFile(rt.src, "README.md", "wip two\n")
	middle, wantMiddle := rt.tick()
	tipB := rt.commit(rt.src, "b.txt", "second unpushed commit\n")
	f.WriteFile(rt.src, "README.md", "wip three\n")
	last, wantLast := rt.tick()
	if len(first.History) != 1 || first.History[0].Tip != tipA || len(middle.History) != 1 || middle.History[0].Artifact != first.History[0].Artifact {
		t.Fatalf("first history %+v middle history %+v, want the middle to reuse tip %s", first.History, middle.History, tipA)
	}
	linkA := first.History[0].Artifact
	if len(last.History) != 2 || last.History[0].Artifact != linkA || last.History[1].Tip != tipB || !slices.Equal(last.History[1].Prerequisites, []string{tipA}) || !slices.Equal(last.Requires, []string{trunk}) {
		t.Fatalf("last history %+v requires %v, want link %s appended on %s", last.History, last.Requires, tipB, tipA)
	}
	linkB := last.History[1].Artifact

	picks := []struct {
		name string
		snap worktree.Snapshot
		want expectation
		held []worktree.ArtifactRef
	}{
		{"first", first, wantFirst, nil},
		{"middle", middle, wantMiddle, []worktree.ArtifactRef{linkA}},
		{"last", last, wantLast, []worktree.ArtifactRef{linkA}},
	}
	for _, p := range picks {
		received, onRecv := rt.ship(p.snap, p.held...)
		rt.assertRestored(p.snap, rt.pickup(rt.store, rt.recv, onRecv, received, worktree.RestoreOptions{Dest: filepath.Join(f.Root, p.name), Fresh: true}), p.want)
	}

	rt.release(middle)
	refs := rt.mirrorRefs()
	for _, s := range []worktree.Snapshot{first, last} {
		if refs[snapshotRef(s, "head")] != s.Head.Commit || refs[snapshotRef(s, "index")] == "" {
			t.Fatalf("releasing the middle snapshot dropped %s: %v", worktree.SnapshotKey(s), refs)
		}
	}
	if _, ok := refs[snapshotRef(middle, "head")]; ok || refs[worktree.TipPrefix+tipA] != tipA || refs[worktree.TipPrefix+tipB] != tipB {
		t.Fatalf("mirror refs after releasing the middle snapshot = %v", refs)
	}
	if pins := rt.pins(); !slices.Equal(pins, []string{trunk}) {
		t.Fatalf("pins after releasing the middle snapshot = %v, want [%s]", pins, trunk)
	}

	rt.gc()
	for _, p := range picks {
		received, onRecv := rt.ship(p.snap, linkA, linkB)
		if v := rt.verifyFrom(onRecv, received, worktree.VerifyOptions{}); !v.Ready || len(v.Missing) != 0 {
			t.Fatalf("verify %s from mirrored tips alone after gc = %+v, want ready", p.name, v)
		}
	}
	received, onRecv := rt.ship(last, linkA, linkB)
	rt.assertRestored(last, rt.pickup(rt.store, rt.recv, onRecv, received, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "after-gc"), Fresh: true}), wantLast)
}

func TestLifecycleFetchGateDeferredThenAllowed(t *testing.T) {
	f := vcstest.New(t)
	rt := newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src")), f.GitClone(filepath.Join(f.Root, "recv")))
	trunk := f.AdvanceOrigin("trunk moves past the receiver")
	f.RunGit(rt.src, "pull", "-q")
	rt.commit(rt.src, "feature.txt", "feature\n")
	f.WriteFile(rt.src, "README.md", "wip\n")
	snap, want := rt.tick()
	if !slices.Equal(snap.Requires, []string{trunk}) {
		t.Fatalf("requires %v, want [%s]", snap.Requires, trunk)
	}
	dest := filepath.Join(f.Root, "recovered")

	calls := 0
	if v := rt.verifyFrom(rt.art, snap, worktree.VerifyOptions{FetchOrigin: countGate(&calls, deferFetch)}); v.Ready || !slices.Equal(v.Missing, []string{trunk}) || calls != 1 {
		t.Fatalf("deferred verify = %+v after %d gate calls, want missing [%s] after 1", v, calls, trunk)
	}
	var notReady *worktree.NotReadyError
	if _, err := rt.store.Restore(t.Context(), rt.recvReg(), snap, rt.art, worktree.RestoreOptions{Dest: dest}); !errors.As(err, &notReady) || !slices.Equal(notReady.Missing, []string{trunk}) {
		t.Fatalf("restore before the fetch = %v, want NotReadyError missing [%s]", err, trunk)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unready restore created %s: %v", dest, err)
	}
	if pins, refs := rt.pins(), rt.mirrorRefs(); len(pins) != 0 || len(refs) != 0 {
		t.Fatalf("deferred verify left pins %v mirror refs %v", pins, refs)
	}

	if v := rt.verifyFrom(rt.art, snap, worktree.VerifyOptions{FetchOrigin: countGate(&calls, allowFetch)}); !v.Ready || len(v.Missing) != 0 || calls != 2 {
		t.Fatalf("allowed verify = %+v after %d gate calls, want ready after 2", v, calls)
	}
	if got := rt.git(rt.recv, "rev-parse", "origin/main"); got != trunk {
		t.Fatalf("receiver origin/main = %s, want the fetched %s", got, trunk)
	}

	f.RunGit(rt.recv, "remote", "set-url", "origin", filepath.Join(f.Root, "unreachable.git"))
	if v := rt.verifyFrom(rt.art, snap, worktree.VerifyOptions{FetchOrigin: countGate(&calls, allowFetch)}); !v.Ready || calls != 2 {
		t.Fatalf("verify with the trunk present = %+v after %d gate calls, want ready with no refetch", v, calls)
	}
	rt.assertRestored(snap, rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: dest}), want)
	if pins := rt.pins(); !slices.Equal(pins, []string{trunk}) {
		t.Fatalf("pins = %v, want [%s]", pins, trunk)
	}
}

func TestLifecycleLFSPickupFetchGateDeferredThenAllowed(t *testing.T) {
	f := vcstest.New(t)
	f.EnableLFS("*.bin")
	f.AdvanceOriginPath("base.bin", lfsBase)
	recv := f.GitClone(filepath.Join(f.Root, "recv"))
	f.InstallLFS(recv)
	rt := newRoundTrip(t, f, f.LFSClone(filepath.Join(f.Root, "src")), recv)
	if base := vcstest.LFSObjectPath(filepath.Join(recv, ".git"), vcstest.SHA256([]byte(lfsBase))); f.FileExists(filepath.Dir(base), filepath.Base(base)) {
		t.Fatalf("receiver already holds the base asset at %s", base)
	}
	staged := "staged asset\x00\x07"
	f.WriteFile(rt.src, "README.md", "wip in an lfs repo\n")
	f.WriteFile(rt.src, "new.bin", staged)
	f.RunGit(rt.src, "add", "new.bin")
	snap, want := rt.tick()
	if snap.LFS == nil || !slices.Equal(lfsOIDs(snap), []string{vcstest.SHA256([]byte(staged))}) {
		t.Fatalf("lfs %+v objects %v, want the staged asset shipped", snap.LFS, lfsOIDs(snap))
	}

	calls := 0
	deferred := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "deferred"), FetchLFS: countGate(&calls, deferFetch)})
	if calls != 1 || deferred.Exact || !slices.Equal(deferred.LFSPending, []string{"base.bin"}) || !slices.Equal(deferred.Differences, []string{"base.bin: lfs object pending"}) {
		t.Fatalf("deferred pickup = %+v after %d gate calls, want base.bin pending after 1", deferred, calls)
	}
	if got := f.ReadFile(deferred.Path, "base.bin"); !strings.HasPrefix(got, "version https://git-lfs.github.com/spec/v1") {
		t.Fatalf("deferred base.bin = %q, want an unhydrated pointer", got)
	}
	if got := f.ReadFile(deferred.Path, "new.bin"); got != staged {
		t.Fatalf("deferred new.bin = %q, want the shipped asset", got)
	}

	allowed := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "allowed"), Fresh: true, FetchLFS: countGate(&calls, allowFetch)})
	if calls != 2 {
		t.Fatalf("allowed pickup made %d gate calls in all, want 2", calls)
	}
	rt.assertRestored(snap, allowed, want)

	again := rt.pickup(rt.store, rt.recv, rt.art, snap, worktree.RestoreOptions{Dest: filepath.Join(f.Root, "again"), Fresh: true, FetchLFS: countGate(&calls, allowFetch)})
	if calls != 2 {
		t.Fatalf("pickup with the base asset local made %d gate calls in all, want no refetch", calls)
	}
	rt.assertRestored(snap, again, want)
}

func TestLifecycleConcurrentVerifyOnOneMirror(t *testing.T) {
	f := vcstest.New(t)
	trunk := f.OriginMain()
	recv := f.GitClone(filepath.Join(f.Root, "recv"))
	rts := []*roundTrip{
		newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src-a")), recv),
		newRoundTrip(t, f, f.GitClone(filepath.Join(f.Root, "src-b")), recv),
	}
	snaps := make([]worktree.Snapshot, len(rts))
	wants := make([]expectation, len(rts))
	for i, rt := range rts {
		name := filepath.Base(rt.src)
		rt.commit(rt.src, name+".txt", "unpushed on "+name+"\n")
		f.WriteFile(rt.src, "staged.txt", "staged on "+name+"\n")
		f.RunGit(rt.src, "add", "staged.txt")
		snaps[i], wants[i] = rt.tick()
	}
	if snaps[0].Worktree.ID == snaps[1].Worktree.ID || snaps[0].Head.Commit == snaps[1].Head.Commit {
		t.Fatalf("snapshots share worktree %s or head %s", snaps[0].Worktree.ID, snaps[0].Head.Commit)
	}

	verified := make([]worktree.Verification, len(rts))
	errs := make([]error, len(rts))
	var wg sync.WaitGroup
	for i, rt := range rts {
		wg.Go(func() {
			verified[i], errs[i] = rt.store.Verify(t.Context(), rt.recvReg(), snaps[i], rt.art, worktree.VerifyOptions{})
		})
	}
	wg.Wait()
	for i := range rts {
		if errs[i] != nil || !verified[i].Ready || len(verified[i].Missing) != 0 {
			t.Fatalf("concurrent verify of %s = %+v, %v; want ready", worktree.SnapshotKey(snaps[i]), verified[i], errs[i])
		}
	}
	refs := rts[0].mirrorRefs()
	for _, s := range snaps {
		if refs[snapshotRef(s, "head")] != s.Head.Commit || refs[snapshotRef(s, "index")] == "" || refs[worktree.TipPrefix+s.Head.Commit] != s.Head.Commit {
			t.Fatalf("mirror refs after concurrent verify miss %s: %v", worktree.SnapshotKey(s), refs)
		}
	}
	if pins := rts[0].pins(); !slices.Equal(pins, []string{trunk}) {
		t.Fatalf("pins after concurrent verify = %v, want [%s]", pins, trunk)
	}
	for i, rt := range rts {
		rt.assertRestored(snaps[i], rt.pickup(rt.store, recv, rt.art, snaps[i], worktree.RestoreOptions{Dest: filepath.Join(f.Root, "recovered-"+filepath.Base(rt.src))}), wants[i])
	}

	for i, rt := range rts {
		wg.Go(func() { errs[i] = rt.store.Release(t.Context(), rt.recvReg(), snaps[i]) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent release: %v", err)
	}
	if pins, refs := rts[0].pins(), rts[0].mirrorRefs(); len(pins) != 0 || len(refs) != 0 {
		t.Fatalf("after releasing both pins %v mirror refs %v, want none", pins, refs)
	}
}
