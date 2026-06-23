// Package hostregistry is the public, repo-agnostic host registry: it detects how
// peers reach this machine, runs commands locally and over ssh, discovers
// candidate hosts on the network, verifies their reposync install, and persists
// the self/hosts identity into the shared state.json under a cross-process flock.
//
// It owns only the host-identity slice of state.json (self, hosts); reposync's
// repo-specific keys (repos, settings, default_location) are preserved
// byte-for-byte across an Update so two packages can share one locked file.
package hostregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const (
	configSubdir = "reposync"
	stateFile    = "state.json"
	lockFile     = "reconcile.lock"
	sockFile     = "rpc.sock"

	lockRetryDelay = 200 * time.Millisecond
)

// ErrLockBusy is returned when the reconcile lock is held past the caller's deadline.
var ErrLockBusy = errors.New("reconcile lock held by another process")

// Dir returns the reposync config directory under XDG_CONFIG_HOME or ~/.config.
func Dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, configSubdir), nil
}

// Path returns the absolute path to the state.json file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stateFile), nil
}

// SockPath returns the absolute path to the daemon's RPC unix socket.
func SockPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sockFile), nil
}

// WithLock runs fn while holding an exclusive flock on the reconcile lock file,
// giving up with ErrLockBusy once ctx is done so a contended acquire fails fast
// instead of blocking on a wedged holder. Every cross-package writer of
// state.json acquires this one canonical lock so writers stay serialized.
func WithLock(ctx context.Context, fn func() error) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	lock := flock.New(filepath.Join(dir, lockFile))
	locked, err := lock.TryLockContext(ctx, lockRetryDelay)
	if !locked {
		return fmt.Errorf("%w: %w", ErrLockBusy, err)
	}
	defer func() { _ = lock.Unlock() }()
	return fn()
}

// save writes raw to state.json atomically: a temp file in the state dir renamed
// over the target.
func save(raw []byte) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename state into place: %w", err)
	}
	return nil
}

// readRaw reads state.json as a key-ordered-agnostic map of raw JSON values,
// returning an empty map when the file does not yet exist.
func readRaw() (map[string]json.RawMessage, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is reposync's own state file under the fixed config dir, not user-supplied.
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return raw, nil
}
