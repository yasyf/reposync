package worktree_test

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRestoreLFSFetchScopeCancelled(t *testing.T) {
	h, snap, lfs := lfsFetchHarness(t)
	pids := h.traceLFSProcesses()
	scope := func(ctx context.Context) (context.Context, context.CancelFunc) {
		fetchCtx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-lfs.started:
				cancel()
			case <-fetchCtx.Done():
			}
		}()
		return fetchCtx, cancel
	}
	dest := filepath.Join(h.f.Root, "recovered")
	r, err := h.store.Restore(t.Context(), h.recvReg(), snap, h.art, worktree.RestoreOptions{Dest: dest, FetchLFS: true, FetchLFSContext: scope})
	if err != nil {
		t.Fatalf("restore with a cancelled lfs fetch: %v", err)
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
	opts := worktree.RestoreOptions{Dest: dest, FetchLFS: true, FetchLFSContext: context.WithCancel}
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
