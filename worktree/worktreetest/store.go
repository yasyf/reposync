// Package worktreetest provides an in-memory worktree.ArtifactSink and
// worktree.ArtifactSource with Put accounting and fault injection for tests of
// capture, verify, and restore.
package worktreetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"sync"

	"github.com/yasyf/reposync/worktree"
)

// Store is an in-memory content-addressed artifact store. Faults apply to Open
// only, so a store can serve bytes that break the ArtifactSource contract and
// prove the consumer's own checks catch it.
type Store struct {
	// OnPut, when set before use, runs after every Put stores its bytes.
	OnPut func(worktree.ArtifactRef)

	mu       sync.Mutex
	objects  map[string][]byte
	puts     []worktree.ArtifactRef
	corrupt  map[string]bool
	truncate map[string]int
}

// New returns an empty Store.
func New() *Store {
	return &Store{objects: map[string][]byte{}, corrupt: map[string]bool{}, truncate: map[string]int{}}
}

// Has reports whether each ref's digest is stored with a matching size.
func (s *Store) Has(ctx context.Context, refs []worktree.ArtifactRef) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	has := make([]bool, len(refs))
	for i, r := range refs {
		data, ok := s.objects[r.Digest]
		has[i] = ok && int64(len(data)) == r.Size
	}
	return has, nil
}

// Put reads r to EOF, stores the bytes under their sha256 digest (idempotent
// for an existing digest), and records the call.
func (s *Store) Put(ctx context.Context, media worktree.Media, r io.Reader) (worktree.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return worktree.ArtifactRef{}, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return worktree.ArtifactRef{}, fmt.Errorf("read artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	ref := worktree.ArtifactRef{Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data)), Media: media}
	s.mu.Lock()
	if _, ok := s.objects[ref.Digest]; !ok {
		s.objects[ref.Digest] = data
	}
	s.puts = append(s.puts, ref)
	s.mu.Unlock()
	if s.OnPut != nil {
		s.OnPut(ref)
	}
	return ref, nil
}

// Open returns the stored bytes for ref, altered by any injected fault. A
// missing ref's error wraps fs.ErrNotExist.
func (s *Store) Open(ctx context.Context, ref worktree.ArtifactRef) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[ref.Digest]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", ref.Digest, fs.ErrNotExist)
	}
	data = bytes.Clone(data)
	if n, ok := s.truncate[ref.Digest]; ok && n < len(data) {
		data = data[:n]
	}
	if s.corrupt[ref.Digest] && len(data) > 0 {
		data[len(data)/2] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Puts returns every Put call's resulting ref in call order, including
// idempotent re-Puts of stored content.
func (s *Store) Puts() []worktree.ArtifactRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]worktree.ArtifactRef(nil), s.puts...)
}

// PutsOf counts the Put calls made with media.
func (s *Store) PutsOf(media worktree.Media) int {
	n := 0
	for _, r := range s.Puts() {
		if r.Media == media {
			n++
		}
	}
	return n
}

// ResetPuts clears the Put call record.
func (s *Store) ResetPuts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = nil
}

// Len is the number of distinct stored digests.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

// Corrupt makes Open flip a byte in the middle of the content it yields for
// ref.
func (s *Store) Corrupt(ref worktree.ArtifactRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corrupt[ref.Digest] = true
}

// Truncate makes Open yield only the first n bytes of ref's content.
func (s *Store) Truncate(ref worktree.ArtifactRef, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncate[ref.Digest] = n
}

// Remove deletes ref's content, so Has reports false and Open fails.
func (s *Store) Remove(ref worktree.ArtifactRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, ref.Digest)
}

// CopyTo copies refs' stored bytes into dst, bypassing faults and Put
// accounting, to relay a snapshot's artifacts to another host's store.
func (s *Store) CopyTo(dst *Store, refs []worktree.ArtifactRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst.mu.Lock()
	defer dst.mu.Unlock()
	for _, r := range refs {
		data, ok := s.objects[r.Digest]
		if !ok {
			return fmt.Errorf("copy %s: %w", r.Digest, fs.ErrNotExist)
		}
		dst.objects[r.Digest] = bytes.Clone(data)
	}
	return nil
}

var (
	_ worktree.ArtifactSink   = (*Store)(nil)
	_ worktree.ArtifactSource = (*Store)(nil)
)
