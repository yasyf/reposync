package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yasyf/daemonkit/durable"
)

const lockWait = 10 * time.Second

// Store is the host-local state under one root: per-repo locks, source-side
// capture ledgers, and receiver-side mirrors.
type Store struct {
	root string
}

// OpenStore opens (creating as needed) the store rooted at the absolute path root.
func OpenStore(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("store root %q is not absolute", root)
	}
	for _, d := range []string{root, filepath.Join(root, "locks"), filepath.Join(root, "mirror")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create store dir: %w", err)
		}
	}
	return &Store{root: root}, nil
}

func repoKey(origin string) string {
	sum := sha256.Sum256([]byte(origin))
	return hex.EncodeToString(sum[:])[:16]
}

func (s *Store) lockRepo(ctx context.Context, origin string) (*durable.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	l, err := durable.AcquireLock(ctx, filepath.Join(s.root, "locks", repoKey(origin)+".lock"))
	if errors.Is(err, durable.ErrLockBusy) {
		return nil, fmt.Errorf("%w: %w", ErrBusy, err)
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", origin, err)
	}
	return l, nil
}
