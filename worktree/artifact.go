// Package worktree captures a git or jj worktree's uncommitted and unpublished
// work as content-addressed artifacts plus a canonical snapshot manifest, and
// restores it on another host into a fresh recovery worktree. It never writes
// the source repository.
package worktree

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Media labels an artifact's content type.
type Media string

const (
	// MediaBundle is a git bundle carrying unpublished history; it is already
	// zlib/delta-compressed.
	MediaBundle Media = "application/x-git-bundle"
	// MediaBlob is a staged blob's raw bytes, with no filters applied.
	MediaBlob Media = "application/vnd.git.blob"
	// MediaFile is a worktree file's raw bytes or a symlink's target.
	MediaFile Media = "application/octet-stream"
	// MediaLFSObject is a git-lfs object's bytes; its digest equals the LFS oid.
	MediaLFSObject Media = "application/vnd.git-lfs.object"
	// MediaSnapshot is an Encode(Snapshot) manifest.
	MediaSnapshot Media = "application/vnd.reposync.worktree-snapshot+json"
)

const digestPrefix = "sha256:"

// ArtifactRef names stored content by its sha256 digest ("sha256:<64 hex>"),
// byte size, and media.
type ArtifactRef struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Media  Media  `json:"media"`
}

// ArtifactSink stores captured content. Put hashes while writing, is durable
// before returning, and is idempotent for an existing digest.
type ArtifactSink interface {
	Has(ctx context.Context, refs []ArtifactRef) ([]bool, error)
	Put(ctx context.Context, media Media, r io.Reader) (ArtifactRef, error)
}

// ArtifactSource reads verified content. Open yields exactly ref.Size bytes
// whose sha256 matches ref.Digest, or an error.
type ArtifactSource interface {
	Has(ctx context.Context, refs []ArtifactRef) ([]bool, error)
	Open(ctx context.Context, ref ArtifactRef) (io.ReadCloser, error)
}

func (r ArtifactRef) validate(media Media) error {
	if r.Media != media {
		return fmt.Errorf("media %q, want %q", r.Media, media)
	}
	if r.Size < 0 {
		return fmt.Errorf("negative size %d", r.Size)
	}
	hexDigest, ok := strings.CutPrefix(r.Digest, digestPrefix)
	if !ok || !isHex(hexDigest, 64) {
		return fmt.Errorf("malformed digest %q", r.Digest)
	}
	return nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
