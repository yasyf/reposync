package env

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yasyf/synckit/cregistry"
)

// ScanNames returns the names of regular files directly in root matching the env
// pattern (".env" exactly or a ".env." prefix), sorted. Symlinks and directories
// are excluded.
func ScanNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("scan env files in %s: %w", root, err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !matchesPattern(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", filepath.Join(root, name), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// maxFileNameLen caps a wire file name at the common filesystem byte limit.
const maxFileNameLen = 255

// ValidateFileName guards a name arriving over the wire: it must be a bare filename
// (no path separators, no "..") under maxFileNameLen bytes, free of control bytes,
// that matches the env pattern.
func ValidateFileName(name string) error {
	if filepath.Base(name) != name {
		return fmt.Errorf("env file name %q is not a bare filename", name)
	}
	if len(name) > maxFileNameLen {
		return fmt.Errorf("env file name is %d bytes, over the %d limit", len(name), maxFileNameLen)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("env file name %q contains a control byte", name)
		}
	}
	if !matchesPattern(name) {
		return fmt.Errorf("env file name %q does not match the .env pattern", name)
	}
	return nil
}

// matchesPattern reports whether name is ".env" or has a ".env." prefix.
func matchesPattern(name string) bool {
	return name == ".env" || strings.HasPrefix(name, ".env.")
}

// Exempt reports whether name under root is exempt from env sync on this host: its
// path Lstats to something that exists but is not a regular file (a symlink or
// directory), or to a regular file larger than MaxFileSize. Exempt names are never
// observed, written, or propagated. It never follows symlinks; a missing path is not
// exempt.
func Exempt(root, name string) (bool, error) {
	exempt, _, err := exemptInfo(filepath.Join(root, name))
	return exempt, err
}

// exemptInfo Lstats path and reports whether it is exempt (see Exempt), returning the
// Lstat result so callers reuse it. A missing path returns (false, nil, nil).
func exemptInfo(path string) (bool, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("lstat env file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return true, info, nil
	}
	return info.Size() > MaxFileSize, info, nil
}

// exemptReason describes why info is exempt, for a stderr note.
func exemptReason(info os.FileInfo) string {
	if info.Mode().IsRegular() {
		return fmt.Sprintf("oversized file, %d bytes", info.Size())
	}
	return "non-regular file"
}

// Observe folds the current on-disk state of each env file into a copy of sc's
// registries and returns the result. It is read-only and idempotent: stamps derive
// from file mtimes, so the same files and sidecar always yield deeply-equal output.
// For each file in the union of names and sidecar files: keys present with a
// differing value are added, present sidecar keys gone from the file are removed, a
// file that vanished after being synced has its present keys tombstoned at the root
// directory's mtime, and a file that is missing or empty and never synced is left
// alone. An exempt name (see Exempt: a symlink or other non-regular path, or a
// regular file larger than MaxFileSize) folds to nothing — not even a tombstone,
// since it is a deliberate local arrangement — with a note to stderr.
func Observe(sc Sidecar, root string, names []string) (RepoState, error) {
	out := make(RepoState, len(sc.Files))
	for name, reg := range sc.Files {
		out[name] = maps.Clone(reg)
	}
	for _, name := range union(names, sc.Files) {
		reg := out[name]
		path := filepath.Join(root, name)
		exempt, info, err := exemptInfo(path)
		if err != nil {
			return nil, err
		}
		if info == nil {
			if err := observeMissing(out, name, root); err != nil {
				return nil, err
			}
			continue
		}
		if exempt {
			fmt.Fprintf(os.Stderr, "reposync/env: skipping %s: %s\n", path, exemptReason(info))
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: env file under a reposync-tracked repo root, not user-supplied.
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", path, err)
		}
		kv := parse(data).values()
		if reg == nil {
			if len(kv) == 0 {
				continue
			}
			reg = cregistry.New[string]()
			out[name] = reg
		}
		mtime := info.ModTime()
		for k, v := range kv {
			if e, ok := reg[k]; ok && e.Present() && e.Value == v {
				continue
			}
			reg.Add(k, v, stampFor(mtime, reg[k]))
		}
		for k, e := range reg.Present() {
			if _, inFile := kv[k]; !inFile {
				reg.Remove(k, stampFor(mtime, e))
				clearRemoved(reg, k)
			}
		}
	}
	return out, nil
}

func clearRemoved(reg FileMap, k string) {
	e := reg[k]
	e.Value = ""
	reg[k] = e
}

// observeMissing tombstones the present keys of a file that has vanished from disk,
// stamping at the root directory's mtime. A file that was never synced is a no-op.
func observeMissing(out RepoState, name, root string) error {
	reg := out[name]
	if reg == nil {
		return nil
	}
	present := reg.Present()
	if len(present) == 0 {
		return nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat repo root %s: %w", root, err)
	}
	mtime := info.ModTime()
	for k, e := range present {
		reg.Remove(k, stampFor(mtime, e))
		clearRemoved(reg, k)
	}
	return nil
}

// union returns the names in scanned followed by any sidecar file names not already
// present, deterministically ordered so stderr notes and iteration stay stable.
func union(scanned []string, files RepoState) []string {
	seen := make(map[string]bool, len(scanned)+len(files))
	out := make([]string, 0, len(scanned)+len(files))
	for _, n := range scanned {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	extra := make([]string, 0, len(files))
	for n := range files {
		if !seen[n] {
			seen[n] = true
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// stampFor derives a regression-safe stamp from mtime for a mutation on entry.
// Normally it is the file's mtime in microseconds; when that does not exceed the
// entry's dominant stamp (an mtime regression, e.g. a restored old file), it bumps
// to one past that stamp so cregistry's monotone guard still takes the local change.
func stampFor(mtime time.Time, entry cregistry.Entry[string]) cregistry.Micros {
	at := cregistry.UnixMicros(mtime)
	floor := entry.Added
	if entry.Removed > floor {
		floor = entry.Removed
	}
	if at <= floor {
		at = floor + 1
	}
	return at
}
