package watch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchSetColocatedJJ(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(abs, ".jj"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := watchSet(abs)
	want := []string{
		filepath.Join(abs, ".jj", "repo", "op_heads", "heads"),
		filepath.Join(abs, ".git", "refs", "remotes", "origin"),
		filepath.Join(abs, ".git", "logs", "refs", "remotes", "origin"),
	}
	assertPaths(t, got, want)
}

func TestWatchSetPlainGit(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "repo") // no .jj on disk
	got := watchSet(abs)
	want := []string{
		filepath.Join(abs, ".git", "refs", "remotes", "origin"),
		filepath.Join(abs, ".git", "logs", "refs", "remotes", "origin"),
	}
	assertPaths(t, got, want)
}

// TestWatchmanObservesRefChange is the one integration test against real
// watchman: subscribe to a ref directory, mutate a ref after the subscription
// clock, and assert a subscription PDU naming that subscription arrives. It is
// bounded by a timeout so it can never hang.
func TestWatchmanObservesRefChange(t *testing.T) {
	requireWatchman(t)

	dir, err := os.MkdirTemp("/tmp", "wm")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// watchman rejects a symlinked root (macOS /tmp -> /private/tmp), so resolve it.
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	refdir := filepath.Join(dir, ".git", "refs", "remotes", "origin")
	if err := os.MkdirAll(refdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refdir, "main"), []byte("aaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wm, err := dialWatchman(ctx)
	if err != nil {
		t.Fatalf("dial watchman: %v", err)
	}
	t.Cleanup(func() {
		wm.send("watch-del", refdir)
		wm.close()
	})

	const subName = "reposync:test:0"
	events := make(chan string, 8)
	dispatch := func(pdu map[string]json.RawMessage) {
		var name string
		json.Unmarshal(pdu["subscription"], &name)
		select {
		case events <- name:
		default:
		}
	}
	if err := wm.subscribe(refdir, subName, dispatch); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go wm.runSubscriptions(ctx, dispatch)

	// Mutate the ref after the subscription clock; expect a subscription PDU.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(refdir, "main"), []byte("bbbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case name := <-events:
		if name != subName {
			t.Errorf("subscription name = %q, want %q", name, subName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no watchman subscription event observed within 5s")
	}
}

func requireWatchman(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("watchman"); err != nil {
		t.Skipf("watchman not installed: %v", err)
	}
	if out, err := exec.Command("watchman", "version").CombinedOutput(); err != nil {
		t.Fatalf("watchman version failed: %v: %s", err, out)
	}
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
