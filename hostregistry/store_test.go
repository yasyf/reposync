package hostregistry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestWithLockRunsFnAndCreatesLockFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	ran := false
	if err := WithLock(context.Background(), func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !ran {
		t.Error("WithLock: fn did not run")
	}

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); err != nil {
		t.Errorf("lock file missing: %v", err)
	}

	if err := WithLock(context.Background(), func() error { return nil }); err != nil {
		t.Errorf("second WithLock: %v", err)
	}
}

func TestWithLockContendedReturnsErrLockBusy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Hold the lock via an independent flock handle on the same file, simulating
	// another process holding the reconcile lock.
	holder := flock.New(filepath.Join(dir, lockFile))
	locked, err := holder.TryLock()
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	if !locked {
		t.Fatal("could not acquire lock to hold")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	ran := false
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- WithLock(ctx, func() error {
			ran = true
			return nil
		})
	}()

	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("contended WithLock took %s, want fast failure", elapsed)
		}
		if !errors.Is(err, ErrLockBusy) {
			t.Fatalf("WithLock err = %v, want ErrLockBusy", err)
		}
		if ran {
			t.Error("fn ran despite the lock being held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contended WithLock blocked past its deadline")
	}

	// Release the held lock; a fresh acquire must now succeed.
	if err := holder.Unlock(); err != nil {
		t.Fatalf("release held lock: %v", err)
	}
	acquired := false
	if err := WithLock(context.Background(), func() error {
		acquired = true
		return nil
	}); err != nil {
		t.Fatalf("WithLock after release: %v", err)
	}
	if !acquired {
		t.Error("fn did not run after the lock was released")
	}
}

func TestPathAndSockPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, stateFile) {
		t.Errorf("Path = %q, want %q", path, filepath.Join(dir, stateFile))
	}
	sock, err := SockPath()
	if err != nil {
		t.Fatal(err)
	}
	if sock != filepath.Join(dir, sockFile) {
		t.Errorf("SockPath = %q, want %q", sock, filepath.Join(dir, sockFile))
	}
}
