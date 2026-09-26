package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/reposync/internal/vcs"
)

const (
	lockWait     = 10 * time.Second
	ledgerSchema = "reposync-worktree-ledger-v1"
)

// Store is reposync's private on-disk state for worktree capture and restore:
// per-repo locks, the source-side scratch repositories and capture ledgers, and
// the receiver-side mirrors. Nothing under it is part of any user repository.
type Store struct {
	root string
}

// OpenStore opens (creating as needed) the store rooted at root.
func OpenStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve store root %s: %w", root, err)
	}
	for _, dir := range []string{"locks", "source", "mirror", "tmp"} {
		if err := os.MkdirAll(filepath.Join(abs, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create store dir: %w", err)
		}
	}
	return &Store{root: abs}, nil
}

func repoKey(origin string) string {
	sum := sha256.Sum256([]byte(origin))
	return hex.EncodeToString(sum[:])[:16]
}

func (s *Store) lockPath(origin string) string {
	return filepath.Join(s.root, "locks", repoKey(origin)+".lock")
}

func (s *Store) scratchDir(origin string) string {
	return filepath.Join(s.root, "source", repoKey(origin)+".git")
}

func (s *Store) ledgerPath(worktreeID string) string {
	return filepath.Join(s.root, "source", worktreeID+".json")
}

func (s *Store) privateIndexPath(worktreeID string) string {
	return filepath.Join(s.root, "source", worktreeID+".index")
}

func (s *Store) mirrorDir(origin string) string {
	return filepath.Join(s.root, "mirror", repoKey(origin)+".git")
}

func (s *Store) mirrorLedgerPath(origin string) string {
	return filepath.Join(s.root, "mirror", repoKey(origin)+".json")
}

func (s *Store) tempDir() string {
	return filepath.Join(s.root, "tmp")
}

func (s *Store) lockRepo(ctx context.Context, origin string) (*durable.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	lock, err := durable.AcquireLock(ctx, s.lockPath(origin))
	if errors.Is(err, durable.ErrLockBusy) {
		return nil, fmt.Errorf("%w: store lock for %s: %w", ErrBusy, origin, err)
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", origin, err)
	}
	return lock, nil
}

func initBareAlternate(ctx context.Context, dir, objectFormat, objectsDir string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		if err := storeGit(ctx, "", nil, "init", "-q", "--bare", "--object-format="+objectFormat, dir); err != nil {
			return err
		}
		for _, kv := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}, {"core.hooksPath", os.DevNull}} {
			if err := storeGit(ctx, dir, nil, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	var format bytes.Buffer
	if err := storeGit(ctx, dir, &format, "rev-parse", "--show-object-format"); err != nil {
		return err
	}
	if got := strings.TrimSpace(format.String()); got != objectFormat {
		return fmt.Errorf("%w: %s is %s, snapshot is %s", ErrObjectFormat, dir, got, objectFormat)
	}
	alternates := filepath.Join(dir, "objects", "info", "alternates")
	want := []byte(objectsDir + "\n")
	//nolint:gosec // G304: the alternates file of a store-owned bare repository.
	if got, err := os.ReadFile(alternates); err == nil && bytes.Equal(got, want) {
		return nil
	}
	if err := durable.WriteFile(alternates, want, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", alternates, err)
	}
	return nil
}

func storeGit(ctx context.Context, gitDir string, stdout *bytes.Buffer, args ...string) error {
	if gitDir != "" {
		args = append([]string{"--git-dir=" + gitDir}, args...)
	}
	cmd := vcs.Cmd{Name: "git", Args: args, Env: vcs.ReadOnlyGitEnv()}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if err := vcs.Exec(ctx, cmd); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func readDurable[T durable.Validating](path string, empty T) (T, error) {
	v, err := durable.ReadFile[T](path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return v, fmt.Errorf("read %s: %w", path, err)
	}
	return v, nil
}

func writeDurable[T durable.Validating](path string, v T) error {
	data, err := durable.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := durable.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

type ledger struct {
	Schema   string                `json:"schema"`
	Worktree string                `json:"worktree"`
	Files    map[string]cachedFile `json:"files,omitempty"`
	Blobs    map[string]cachedBlob `json:"blobs,omitempty"`
	Chain    []chainLink           `json:"chain,omitempty"`
}

type fileStat struct {
	Ino     uint64 `json:"ino"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	CtimeNS int64  `json:"ctime_ns"`
	Mode    uint32 `json:"mode"`
}

type cachedFile struct {
	Stat    fileStat    `json:"stat"`
	Content ArtifactRef `json:"content"`
	BlobOID string      `json:"blob_oid,omitempty"`
}

type cachedBlob struct {
	Blob ArtifactRef `json:"blob"`
	LFS  *lfsPointer `json:"lfs,omitempty"`
}

type chainLink struct {
	Bundle   Bundle   `json:"bundle"`
	Excludes []string `json:"excludes"`
	Requires []string `json:"requires"`
}

func newLedger(worktreeID string) ledger {
	return ledger{Schema: ledgerSchema, Worktree: worktreeID}
}

func (l ledger) Validate() error {
	if l.Schema != ledgerSchema {
		return fmt.Errorf("ledger schema %q, want %q", l.Schema, ledgerSchema)
	}
	if l.Worktree == "" {
		return fmt.Errorf("ledger without worktree")
	}
	for p, f := range l.Files {
		if err := f.Content.validate(MediaFile); err != nil {
			return fmt.Errorf("ledger file %q: %w", p, err)
		}
	}
	for oid, b := range l.Blobs {
		if err := b.Blob.validate(MediaBlob); err != nil {
			return fmt.Errorf("ledger blob %s: %w", oid, err)
		}
	}
	for i, c := range l.Chain {
		if err := c.Bundle.Artifact.validate(MediaBundle); err != nil {
			return fmt.Errorf("ledger chain[%d]: %w", i, err)
		}
	}
	return nil
}

func (s *Store) readLedger(worktreeID string) (ledger, error) {
	l, err := readDurable(s.ledgerPath(worktreeID), newLedger(worktreeID))
	if err != nil {
		return ledger{}, err
	}
	if l.Worktree != worktreeID {
		return ledger{}, fmt.Errorf("ledger %s belongs to worktree %s", s.ledgerPath(worktreeID), l.Worktree)
	}
	return l, nil
}
