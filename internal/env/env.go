// Package env is the key-level sync core for root .env files: a per-file
// last-write-wins map over synckit's cregistry, a byte-faithful dotenv parser and
// rewriter, an idempotent mtime-stamped observation fold, and a per-repo sidecar
// that doubles as the 3-way merge base.
//
// Parsing is deliberately narrow and line-based. A line is a key/value assignment
// iff the text left of its first '=' — after trimming surrounding whitespace and
// one optional "export" + whitespace prefix — matches [A-Za-z_][A-Za-z0-9_]*; the
// value is the raw bytes to the right of that first '=', verbatim. Everything else
// (comments, blank lines, malformed or unterminated lines) is layout: never
// parsed, never synced, preserved byte-for-byte on rewrite.
//
// Unsupported by design: multiline quoted values (each newline starts a new line),
// and CRLF normalization (a trailing '\r' stays in the value bytes). Values are
// never unquoted or trimmed — quotes and surrounding spaces are part of the value.
//
// Exempt paths are never observed, written, or propagated from this host: a symlink
// or other non-regular file (a deliberate local arrangement, never followed) and a
// regular file over MaxFileSize both sync as if absent, without tombstoning peers.
package env

import (
	"time"

	"github.com/yasyf/synckit/cregistry"
)

const (
	// MethodGetState is the rpc method a host calls to fetch a peer's stamped env state.
	MethodGetState = "env.get_state"
	// QuietWindow is how long a target file must sit unmodified before an apply may
	// write it, so a converge never races a concurrent local edit.
	QuietWindow = 5 * time.Second
	// TombstoneTTL is how long a removed key is retained before sidecar GC drops it;
	// resurrection requires a replica offline longer than this against the converge cadence.
	TombstoneTTL = 90 * 24 * time.Hour
	// MaxFileSize is the largest env file observed; larger files are skipped.
	MaxFileSize = 256 << 10
)

// Now is the clock the sidecar GC compares tombstone stamps against, indirected so
// tests can pin time; the observation fold never reads it, deriving stamps from file
// mtimes instead.
var Now = time.Now

// FileMap is one file's last-write-wins map of env key to value, tombstones and all.
type FileMap = cregistry.Registry[string]

// RepoState is a repo's env state: each root .env file name mapped to its FileMap.
type RepoState map[string]FileMap

// Sidecar is a repo's persisted env state, keyed by origin. Its present-key set
// always equals the file's KV content at persist time, so it doubles as the 3-way
// merge base for the next converge.
type Sidecar struct {
	Origin string    `json:"origin"`
	Files  RepoState `json:"files"`
}
