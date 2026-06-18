package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/rpc"
	"github.com/yasyf/reposync/internal/state"
)

// TestWatchServesRPCAndCleansSocket boots the daemon, drives a reconcile over its
// RPC socket, then cancels and asserts the socket is unlinked on shutdown. The
// XDG base lives under /tmp so the socket path stays under the sun_path limit, and
// state is persisted there so the server's state.Load never touches the real home.
func TestWatchServesRPCAndCleansSocket(t *testing.T) {
	requireWatchman(t)

	base, err := os.MkdirTemp("/tmp", "rs")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("XDG_CONFIG_HOME", base)

	st := &state.State{DefaultLocation: filepath.Join(base, "data")}
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	sock, err := state.SockPath()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, st) }()

	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil }, "rpc socket to appear")

	resp, err := rpc.Reconcile(ctx, sock)
	if err != nil {
		t.Fatalf("rpc reconcile: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("reconcile response error: %s", resp.Err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("expected no results for an empty repo set, got %v", resp.Results)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return within 5s of cancel")
	}

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("rpc socket not unlinked on shutdown: stat err = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
