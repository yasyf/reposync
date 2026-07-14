package env

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/synckit/cregistry"
)

// sidecarTempPrefix names a sidecar's temp file during an atomic save.
const sidecarTempPrefix = ".reposync-sidecar-"

// sanitizeMax caps the human-readable portion of a sidecar filename; the origin
// hash disambiguates truncated or collapsed names.
const sanitizeMax = 64

// SidecarPath returns the sidecar file for origin under configDir: a sanitized,
// length-capped rendering of origin joined with the first 8 hex of its sha256.
func SidecarPath(configDir, origin string) string {
	sum := sha256.Sum256([]byte(origin))
	short := hex.EncodeToString(sum[:])[:8]
	return filepath.Join(configDir, "env", sanitize(origin)+"-"+short+".json")
}

// sanitize maps origin to a filename-safe ASCII string, replacing any character
// outside [A-Za-z0-9._-] with '-' and capping the length.
func sanitize(origin string) string {
	var b strings.Builder
	for _, r := range origin {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > sanitizeMax {
		out = out[:sanitizeMax]
	}
	return out
}

// LoadSidecar reads the sidecar at path for origin. A missing file yields an empty
// sidecar; an embedded origin that disagrees with origin is a loud error.
func LoadSidecar(path, origin string) (Sidecar, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: sidecar under reposync's own config dir, not user-supplied.
	if err != nil {
		if os.IsNotExist(err) {
			return Sidecar{Origin: origin, Files: make(RepoState)}, nil
		}
		return Sidecar{}, fmt.Errorf("read sidecar %s: %w", path, err)
	}
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return Sidecar{}, fmt.Errorf("parse sidecar %s: %w", path, err)
	}
	if sc.Origin != origin {
		return Sidecar{}, fmt.Errorf("sidecar %s: origin %q does not match %q", path, sc.Origin, origin)
	}
	if sc.Files == nil {
		sc.Files = make(RepoState)
	}
	return sc, nil
}

// Save GCs expired tombstones, drops emptied files, and atomically writes the
// sidecar (dir 0700, file 0600). A tombstone is dropped once it is absent and its
// newest stamp is older than TombstoneTTL.
func (sc Sidecar) Save(path string) error {
	cutoff := cregistry.UnixMicros(Now().Add(-TombstoneTTL))
	files := make(RepoState, len(sc.Files))
	for name, reg := range sc.Files {
		kept := make(FileMap, len(reg))
		for k, e := range reg {
			if !e.Present() && maxStamp(e) < cutoff {
				continue
			}
			kept[k] = e
		}
		if len(kept) == 0 {
			continue
		}
		files[name] = kept
	}
	data, err := json.MarshalIndent(Sidecar{Origin: sc.Origin, Files: files}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create sidecar dir %s: %w", dir, err)
	}
	return atomicWrite(path, data, 0o600, sidecarTempPrefix)
}

// maxStamp returns the newer of an entry's add and remove stamps.
func maxStamp(e cregistry.Entry[string]) cregistry.Micros {
	if e.Removed > e.Added {
		return e.Removed
	}
	return e.Added
}
