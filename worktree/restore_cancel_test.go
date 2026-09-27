package worktree_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/vcs/vcstest"
	"github.com/yasyf/reposync/worktree"
)

type interruptedSource struct {
	worktree.ArtifactSource
	dest      string
	interrupt func() error
}

func (s interruptedSource) Open(ctx context.Context, ref worktree.ArtifactRef) (io.ReadCloser, error) {
	if _, err := os.Lstat(s.dest); err == nil {
		if err := s.interrupt(); err != nil {
			return nil, err
		}
	}
	return s.ArtifactSource.Open(ctx, ref)
}

type receiverState struct {
	heads     map[string]string
	worktrees string
	status    []string
}

func (h *harness) receiverState() receiverState {
	h.t.Helper()
	return receiverState{
		heads:     h.refs([]string{"-C", h.recv}, "refs/heads/"),
		worktrees: h.git(h.recv, "worktree", "list", "--porcelain"),
		status:    h.status(h.recv),
	}
}

func (h *harness) assertRolledBack(before receiverState, dest string) {
	h.t.Helper()
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("failed restore left %s: %v", dest, err)
	}
	after := h.receiverState()
	if !maps.Equal(before.heads, after.heads) {
		h.t.Fatalf("branches changed:\nbefore %v\nafter  %v", before.heads, after.heads)
	}
	if before.worktrees != after.worktrees {
		h.t.Fatalf("worktrees changed:\nbefore %s\nafter  %s", before.worktrees, after.worktrees)
	}
	if !slices.Equal(before.status, after.status) {
		h.t.Fatalf("receiver status changed:\nbefore %q\nafter  %q", before.status, after.status)
	}
	if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
		h.t.Fatalf("recovery refs left in checkout: %v", refs)
	}
}

func TestRestoreRollsBackOnFailure(t *testing.T) {
	errInjected := errors.New("injected artifact failure")
	tests := []struct {
		name      string
		interrupt func(cancel context.CancelFunc) error
		want      error
	}{
		{"cancelled", func(cancel context.CancelFunc) error { cancel(); return nil }, context.Canceled},
		{"failed", func(context.CancelFunc) error { return errInjected }, errInjected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.RunGit(h.recv, "branch", "recovery/taken")
			h.f.RunGit(h.recv, "worktree", "add", "-q", filepath.Join(h.f.Root, "user-worktree"))
			h.f.WriteFile(h.src, "README.md", "edited\n")
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			before := h.receiverState()
			parent := filepath.Join(h.f.Root, "pickups")
			dest := filepath.Join(parent, "repo", "recovered")
			opts := worktree.RestoreOptions{Dest: dest, Branch: "recovery/taken"}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := interruptedSource{ArtifactSource: h.art, dest: dest, interrupt: func() error { return tt.interrupt(cancel) }}
			if _, err := h.store.Restore(ctx, h.recvReg(), snap, src, opts); !errors.Is(err, tt.want) {
				t.Fatalf("restore error = %v, want %v", err, tt.want)
			}
			h.assertRolledBack(before, dest)
			if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed restore left created parent %s: %v", parent, err)
			}

			r := h.restore(snap, opts)
			if r.Path != dest || r.Reused || r.Branch != "recovery/taken-2" {
				t.Fatalf("retry restored %+v, want a fresh recovery/taken-2 at %s", r, dest)
			}
			h.assertFaithful(r)
		})
	}
}

type blockingLFS struct {
	url     string
	started chan struct{}
}

func newBlockingLFS(t *testing.T) *blockingLFS {
	t.Helper()
	b := &blockingLFS{started: make(chan struct{})}
	var once sync.Once
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lfs/objects/batch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Objects []struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"objects"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type action struct {
			Href string `json:"href"`
		}
		type object struct {
			OID           string            `json:"oid"`
			Size          int64             `json:"size"`
			Authenticated bool              `json:"authenticated"`
			Actions       map[string]action `json:"actions"`
		}
		resp := struct {
			Transfer string   `json:"transfer"`
			Objects  []object `json:"objects"`
		}{Transfer: "basic"}
		for _, o := range req.Objects {
			resp.Objects = append(resp.Objects, object{
				OID: o.OID, Size: o.Size, Authenticated: true,
				Actions: map[string]action{"download": {Href: server.URL + "/objects/" + o.OID}},
			})
		}
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode batch response: %v", err)
		}
	})
	mux.HandleFunc("GET /objects/{oid}", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(b.started) })
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
			http.NotFound(w, r)
		}
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	b.url = server.URL + "/lfs"
	return b
}

func (h *harness) traceLFSProcesses() func() []int {
	h.t.Helper()
	gitLFS, err := exec.LookPath("git-lfs")
	if err != nil {
		h.t.Fatal(err)
	}
	bin := h.t.TempDir()
	script := "#!/bin/sh\necho $$ >> '" + filepath.Join(bin, "pids") + "'\nexec '" + gitLFS + "' \"$@\"\n"
	//nolint:gosec // G306: the git-lfs shim must be executable to shadow the real one on PATH.
	if err := os.WriteFile(filepath.Join(bin, "git-lfs"), []byte(script), 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []int {
		h.t.Helper()
		fields := strings.Fields(h.f.ReadFile(bin, "pids"))
		pids := make([]int, 0, len(fields))
		for _, f := range fields {
			pid, err := strconv.Atoi(f)
			if err != nil {
				h.t.Fatal(err)
			}
			pids = append(pids, pid)
		}
		return pids
	}
}

func assertExited(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			if time.Now().After(deadline) {
				t.Fatalf("git-lfs process %d still running", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func lfsFetchHarness(t *testing.T) (*harness, worktree.Snapshot, *blockingLFS) {
	t.Helper()
	f := vcstest.New(t)
	remote := f.EnableLFS("*.bin")
	f.WriteFile(f.Seed, "base.bin", "published base asset")
	f.WriteFile(f.Seed, "other.bin", "another published asset")
	f.RunGit(f.Seed, "add", "base.bin", "other.bin")
	f.RunGit(f.Seed, "commit", "-qm", "base assets")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	h := newHarness(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	h.f.WriteFile(h.recv, "other.bin", h.f.RunGit(h.recv, "cat-file", "blob", "HEAD:other.bin"))
	if err := os.Remove(vcstest.LFSObjectPath(filepath.Join(h.recv, ".git"), vcstest.SHA256([]byte("another published asset")))); err != nil {
		t.Fatal(err)
	}
	lfs := newBlockingLFS(t)
	h.f.RunGit(h.recv, "config", "lfs.url", lfs.url)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.capture()
	snap.LFS = &worktree.LFSInfo{Remote: remote}
	return h, h.seal(snap), lfs
}

func TestRestoreLFSFetchGateInterrupted(t *testing.T) {
	h, snap, lfs := lfsFetchHarness(t)
	pids := h.traceLFSProcesses()
	gate := func(ctx context.Context, fetch func(context.Context) error) error {
		fetchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-lfs.started:
				cancel()
			case <-fetchCtx.Done():
			}
		}()
		return fmt.Errorf("%w: %w", worktree.ErrFetchDeferred, fetch(fetchCtx))
	}
	dest := filepath.Join(h.f.Root, "recovered")
	r, err := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: dest, FetchLFS: gate})
	if err != nil {
		t.Fatalf("restore with an interrupted lfs fetch: %v", err)
	}
	select {
	case <-lfs.started:
	default:
		t.Fatal("lfs fetch never reached the server")
	}
	if r.Path != dest || r.Exact || !slices.Equal(r.LFSPending, []string{"other.bin"}) || !slices.Equal(r.Differences, []string{"other.bin: lfs object pending"}) {
		t.Fatalf("restored %+v, want other.bin pending", r)
	}
	if got := h.f.ReadFile(dest, "other.bin"); !strings.HasPrefix(got, "version https://git-lfs.github.com/spec/v1") {
		t.Fatalf("other.bin = %q, want an unhydrated pointer", got)
	}
	if got := h.f.ReadFile(dest, "base.bin"); got != "published base asset" {
		t.Fatalf("base.bin = %q, want the hydrated local object", got)
	}
	assertExited(t, pids())
}

func TestRestoreCancelledDuringLFSFetch(t *testing.T) {
	h, snap, lfs := lfsFetchHarness(t)
	pids := h.traceLFSProcesses()
	before := h.receiverState()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-lfs.started:
			cancel()
		case <-ctx.Done():
		}
	}()
	dest := filepath.Join(h.f.Root, "recovered")
	opts := worktree.RestoreOptions{Dest: dest, FetchLFS: func(ctx context.Context, fetch func(context.Context) error) error { return fetch(ctx) }}
	if r, err := h.store.Restore(ctx, h.recvReg(), snap, h.art, opts); err == nil {
		t.Fatalf("restore cancelled during its lfs fetch succeeded: %+v", r)
	}
	assertExited(t, pids())
	h.assertRolledBack(before, dest)

	r := h.restore(snap, worktree.RestoreOptions{Dest: dest})
	if r.Path != dest || r.Reused || !slices.Equal(r.LFSPending, []string{"other.bin"}) {
		t.Fatalf("retry restored %+v, want a fresh checkout with other.bin pending", r)
	}
}

type restoreResult struct {
	r   worktree.Restored
	err error
}

func (h *harness) restoreAsync(ctx context.Context, store *worktree.Store, snap worktree.Snapshot, opts worktree.RestoreOptions) <-chan restoreResult {
	done := make(chan restoreResult, 1)
	go func() {
		r, err := store.Restore(ctx, h.recvReg(), snap, h.art, opts)
		done <- restoreResult{r, err}
	}()
	return done
}

type gitPause struct {
	t        *testing.T
	paused   string
	released string
}

func (h *harness) pauseGit(match, input string) *gitPause {
	h.t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		h.t.Fatal(err)
	}
	dir := h.t.TempDir()
	p := &gitPause{t: h.t, paused: filepath.Join(dir, "paused"), released: filepath.Join(dir, "released")}
	cond := "mkdir '" + filepath.Join(dir, "claimed") + "' 2>/dev/null"
	if input != "" {
		cond = "grep -qF -e '" + input + "' \"$in\" && " + cond
	}
	script := strings.Join([]string{
		"#!/bin/sh",
		`case " $* " in`,
		match + ")",
		"\tin='" + filepath.Join(dir, "stdin") + "'.$$",
		"\tcat > \"$in\"",
		"\tif " + cond + "; then",
		"\t\ttouch '" + p.paused + "'",
		"\t\twhile [ ! -e '" + p.released + "' ]; do sleep 0.05; done",
		"\tfi",
		"\texec '" + git + "' \"$@\" < \"$in\"",
		"\t;;",
		"esac",
		"exec '" + git + "' \"$@\"",
	}, "\n") + "\n"
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		h.t.Fatal(err)
	}
	//nolint:gosec // G306: the git shim must be executable to shadow the real one on PATH.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.t.Cleanup(p.release)
	return p
}

func (p *gitPause) wait(done <-chan restoreResult) {
	p.t.Helper()
	deadline := time.After(time.Minute)
	for {
		if _, err := os.Stat(p.paused); err == nil {
			return
		}
		select {
		case res := <-done:
			p.t.Fatalf("restore finished before git paused: %+v, %v", res.r, res.err)
		case <-deadline:
			p.t.Fatal("git never paused")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (p *gitPause) release() {
	p.t.Helper()
	if err := os.WriteFile(p.released, nil, 0o600); err != nil {
		p.t.Fatal(err)
	}
}

func TestRestoreRollbackSparesConcurrentResources(t *testing.T) {
	const (
		beforeFetch = `*" fetch "*":refs/reposync/recovery/"*`
		beforeAdd   = `*" worktree add "*`
	)
	userWorktree := func(h *harness, _ worktree.Snapshot, dest string) (string, string) {
		h.f.RunGit(h.recv, "worktree", "add", "-q", dest)
		h.f.WriteFile(dest, "mine.txt", "user work\n")
		return "refs/heads/recovered", h.git(h.recv, "rev-parse", "HEAD")
	}
	tests := []struct {
		name  string
		match string
		race  func(h *harness, snap worktree.Snapshot, dest string) (ref, oid string)
	}{
		{"worktree before snapshot fetch", beforeFetch, userWorktree},
		{"worktree before worktree add", beforeAdd, userWorktree},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGitHarness(t)
			h.f.WriteFile(h.src, "wip.txt", "wip\n")
			snap := h.seal(h.capture())
			before := h.receiverState()
			dest := filepath.Join(h.f.Root, "recovered")
			p := h.pauseGit(tt.match, "")
			done := h.restoreAsync(t.Context(), h.store, snap, worktree.RestoreOptions{Dest: dest, Branch: "recovery/raced"})
			p.wait(done)
			ref, oid := tt.race(h, snap, dest)
			raced := h.receiverState()
			p.release()
			if res := <-done; res.err == nil {
				t.Fatalf("restore racing another actor succeeded: %+v", res.r)
			}
			after := h.receiverState()
			want := maps.Clone(before.heads)
			want[ref] = oid
			if !maps.Equal(after.heads, want) {
				t.Fatalf("branches after rollback = %v, want %v", after.heads, want)
			}
			if !strings.Contains(after.worktrees, "worktree "+dest+"\n") || h.f.ReadFile(dest, "mine.txt") != "user work\n" {
				t.Fatalf("rollback removed the user's worktree:\nraced %s\nafter %s", raced.worktrees, after.worktrees)
			}
			if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
				t.Fatalf("recovery refs left in checkout: %v", refs)
			}
		})
	}
}

func TestRestoreCancelledDuringFinalCleanup(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())
	before := h.receiverState()
	dest := filepath.Join(h.f.Root, "recovered")
	opts := worktree.RestoreOptions{Dest: dest}
	p := h.pauseGit(tempCleanup, "delete "+worktree.RecoveryPrefix)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.restoreAsync(ctx, h.store, snap, opts)
	p.wait(done)
	cancel()
	p.release()
	if res := <-done; !errors.Is(res.err, context.Canceled) {
		t.Fatalf("restore cancelled during its final cleanup = %+v, %v; want context.Canceled", res.r, res.err)
	}
	h.assertRolledBack(before, dest)
	if r := h.restore(snap, opts); r.Reused || r.Path != dest {
		t.Fatalf("retry restored %+v, want a fresh checkout at %s", r, dest)
	}
}

func TestRestoreStoresKeepTemporaryRefsApart(t *testing.T) {
	h := newGitHarness(t)
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.seal(h.capture())
	other, err := worktree.OpenStore(filepath.Join(h.f.Root, "store-b"))
	if err != nil {
		t.Fatal(err)
	}
	p := h.pauseGit(`*" read-tree -u --reset "*`, "")
	done := h.restoreAsync(t.Context(), h.store, snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "a")})
	p.wait(done)
	b, err := other.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "b")})
	if err != nil {
		t.Fatalf("restore through the second store: %v", err)
	}
	h.assertFaithful(b)
	p.release()
	a := <-done
	if a.err != nil {
		t.Fatalf("restore through the first store: %v", a.err)
	}
	h.assertFaithful(a.r)
	if refs := h.refs([]string{"-C", h.recv}, worktree.RecoveryPrefix); len(refs) != 0 {
		t.Fatalf("recovery refs left in checkout: %v", refs)
	}
}

func (h *harness) limitLFSCheckout(limit int) {
	h.t.Helper()
	gitLFS, err := exec.LookPath("git-lfs")
	if err != nil {
		h.t.Fatal(err)
	}
	bin := h.t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = checkout ]; then ulimit -f " + strconv.Itoa(limit/512) + "; fi\nexec '" + gitLFS + "' \"$@\"\n"
	//nolint:gosec // G306: the git-lfs shim must be executable to shadow the real one on PATH.
	if err := os.WriteFile(filepath.Join(bin, "git-lfs"), []byte(script), 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRestoreRejectsIncompleteLFSCheckout(t *testing.T) {
	f := vcstest.New(t)
	remote := f.EnableLFS("*.bin")
	f.WriteFile(f.Seed, "base.bin", strings.Repeat("published base asset\n", 9000))
	f.RunGit(f.Seed, "add", "base.bin")
	f.RunGit(f.Seed, "commit", "-qm", "base asset")
	f.RunGit(f.Seed, "push", "-q", "origin", "main")
	h := newHarness(t, f, f.LFSClone(filepath.Join(f.Root, "src")), f.LFSClone(filepath.Join(f.Root, "recv")))
	h.f.WriteFile(h.src, "wip.txt", "wip\n")
	snap := h.capture()
	snap.LFS = &worktree.LFSInfo{Remote: remote}
	snap = h.seal(snap)
	h.assertFaithful(h.restore(snap, worktree.RestoreOptions{Dest: filepath.Join(h.f.Root, "unlimited")}))
	before := h.receiverState()
	h.limitLFSCheckout(64 << 10)
	dest := filepath.Join(h.f.Root, "recovered")
	if r, err := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: dest, Fresh: true}); err == nil {
		t.Fatalf("restore over an incomplete lfs checkout succeeded: %+v", r)
	}
	h.assertRolledBack(before, dest)
}
