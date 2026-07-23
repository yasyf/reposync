package env

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tempPrefix names an apply's temp file in the repo root. It must never match the
// scan pattern (".env" / ".env.*") so a half-written temp is never observed as an env file.
const tempPrefix = ".reposync-env-"

// envLine is one parsed line of an env file: its raw text without the trailing
// newline, and — when it is a KV assignment — the key, the verbatim value, and the
// byte index of the first '=' so a rewrite can replace the value while preserving
// everything to its left (indentation and any export prefix).
type envLine struct {
	raw   string
	key   string
	value string
	eq    int
}

// envFile is a parsed env file as an ordered list of lines. Joining the raw fields
// with "\n" reproduces the original bytes exactly.
type envFile struct {
	lines []envLine
}

// parse splits data into lines and classifies each as a KV assignment or layout.
func parse(data []byte) *envFile {
	parts := strings.Split(string(data), "\n")
	lines := make([]envLine, len(parts))
	for i, p := range parts {
		if k, v, eq, ok := classify(p); ok {
			lines[i] = envLine{raw: p, key: k, value: v, eq: eq}
			continue
		}
		lines[i] = envLine{raw: p}
	}
	return &envFile{lines: lines}
}

// classify reports whether raw is a KV assignment and, if so, its key, its verbatim
// value (the bytes right of the first '='), and the index of that '='.
func classify(raw string) (key, value string, eq int, ok bool) {
	eq = strings.IndexByte(raw, '=')
	if eq < 0 {
		return "", "", -1, false
	}
	name := strings.TrimSpace(raw[:eq])
	if rest, cut := strings.CutPrefix(name, "export"); cut {
		if trimmed := strings.TrimLeft(rest, " \t"); len(trimmed) < len(rest) {
			name = trimmed
		}
	}
	if !isName(name) {
		return "", "", -1, false
	}
	return name, raw[eq+1:], eq, true
}

// ValidKey reports whether k is a legal dotenv key: [A-Za-z_][A-Za-z0-9_]*. A merged
// key must pass this so it can never inject an extra line on rewrite.
func ValidKey(k string) bool {
	return isName(k)
}

// ValidValue reports whether v holds no newline, which on rewrite would split into extra
// lines. A merged value must pass this.
func ValidValue(v string) bool {
	return !strings.Contains(v, "\n")
}

// AggregateSize is a file's on-wire byte cost: for every entry, present or tombstoned,
// the key, the value, and the two bytes ('=' and '\n') a written line adds. It is the
// size validatePeerEnv caps a payload at and the merge drops a file over, so both agree
// on when a file exceeds MaxFileSize.
func AggregateSize(reg FileMap) int {
	size := 0
	for k, e := range reg {
		size += len(k) + len(e.Value) + 2
	}
	return size
}

// isName reports whether s matches [A-Za-z_][A-Za-z0-9_]*.
func isName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// values returns the file's effective KV content, last occurrence winning on a
// duplicate key.
func (f *envFile) values() map[string]string {
	m := make(map[string]string)
	for _, l := range f.lines {
		if l.key != "" {
			m[l.key] = l.value
		}
	}
	return m
}

// rewrite produces the file's bytes after reconciling to present and tombstoned:
// each tombstoned key's lines are dropped, each present key whose value differs is
// updated in place at its last occurrence, and present keys absent from the file are
// appended sorted at EOF. Keys in neither set are left untouched.
func (f *envFile) rewrite(present map[string]string, tombstoned map[string]bool) []byte {
	last := make(map[string]int)
	seen := make(map[string]bool)
	for i, l := range f.lines {
		if l.key != "" {
			last[l.key] = i
			seen[l.key] = true
		}
	}
	out := make([]envLine, 0, len(f.lines))
	for i, l := range f.lines {
		if l.key != "" {
			if tombstoned[l.key] {
				continue
			}
			if v, ok := present[l.key]; ok && i == last[l.key] && l.value != v {
				l.raw = l.raw[:l.eq+1] + v
				l.value = v
			}
		}
		out = append(out, l)
	}
	var missing []string
	for k := range present {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	added := make([]envLine, len(missing))
	for i, k := range missing {
		added[i] = envLine{raw: k + "=" + present[k], key: k, value: present[k], eq: len(k)}
	}
	out = insertBeforeTrailingBlank(out, added)
	parts := make([]string, len(out))
	for i, l := range out {
		parts[i] = l.raw
	}
	return []byte(strings.Join(parts, "\n"))
}

// insertBeforeTrailingBlank splices added just before a trailing blank line so a
// file that ended with a newline keeps ending with one; otherwise it appends at EOF.
func insertBeforeTrailingBlank(lines, added []envLine) []envLine {
	if len(added) == 0 {
		return lines
	}
	if n := len(lines); n > 0 && lines[n-1].raw == "" && lines[n-1].key == "" {
		tail := lines[n-1]
		lines = append(lines[:n-1:n-1], added...)
		return append(lines, tail)
	}
	return append(lines, added...)
}

// Apply reconciles the file at path to reg's merged state and reports whether it
// wrote. Present keys are ensured with their merged values, tombstoned keys are
// removed, and new keys are appended sorted. A byte-identical result is not written.
// The file is never deleted; it is created (mode 0600) only when reg has present keys.
// An exempt target (see Exempt: a symlink or other non-regular path, or a file over
// MaxFileSize) is left untouched — the write is skipped without error.
func Apply(path string, reg FileMap) (bool, error) {
	p, err := planApply(path, reg)
	if err != nil {
		return false, err
	}
	return p.write()
}

// applyPlan is the write Apply would perform for one file: the rewritten bytes, the
// mode to write them at, and the path's Lstat (nil when absent). change is false when
// the target is exempt or already byte-identical, so no write is due.
type applyPlan struct {
	path   string
	data   []byte
	mode   os.FileMode
	info   os.FileInfo
	change bool
}

// planApply computes, without writing, the rewrite Apply would perform for reg at path:
// it Lstats path (reusing the exempt check), reads and rewrites an existing file, and
// reports via change whether the result differs from what is on disk.
func planApply(path string, reg FileMap) (applyPlan, error) {
	exempt, info, err := exemptInfo(path)
	if err != nil {
		return applyPlan{}, err
	}
	if exempt {
		return applyPlan{path: path, info: info}, nil
	}
	present := valuesOf(reg.Present())
	tombstoned := tombstonesOf(reg)
	mode := os.FileMode(0o600)
	var data []byte
	if info != nil {
		mode = info.Mode().Perm()
		data, err = os.ReadFile(path) //nolint:gosec // G304: env file under a reposync-tracked repo root, not user-supplied.
		if err != nil {
			return applyPlan{}, fmt.Errorf("read env file %s: %w", path, err)
		}
	} else if len(present) == 0 {
		return applyPlan{path: path, info: info}, nil
	}
	out := parse(data).rewrite(present, tombstoned)
	if info != nil && bytes.Equal(out, data) {
		return applyPlan{path: path, info: info}, nil
	}
	return applyPlan{path: path, data: out, mode: mode, info: info, change: true}, nil
}

// write performs the planned rewrite, reporting whether it wrote.
func (p applyPlan) write() (bool, error) {
	if !p.change {
		return false, nil
	}
	if err := atomicWrite(p.path, p.data, p.mode, tempPrefix); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyAll reconciles every file in merged to disk under root as a two-pass,
// all-or-nothing quiet-window gate, returning (changed, busy, err). Pass 1 plans the
// rewrite of every file and, for each that would change, freshly Lstats it; if any such
// existing file was modified within QuietWindow of now, ApplyAll returns busy having
// written nothing. Pass 2 writes every planned change. The gate and the writes are as
// close as the filesystem allows: a local edit landing in the microseconds between a
// file's pass-1 Lstat and its pass-2 rename is the residual race QuietWindow narrows but
// cannot close.
func ApplyAll(root string, merged RepoState) (bool, bool, error) {
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	now := time.Now()
	plans := make([]applyPlan, 0, len(names))
	for _, name := range names {
		p, err := planApply(filepath.Join(root, name), merged[name])
		if err != nil {
			return false, false, err
		}
		if !p.change {
			continue
		}
		if p.info != nil && now.Sub(p.info.ModTime()) < QuietWindow {
			return false, true, nil
		}
		plans = append(plans, p)
	}
	changed := false
	for _, p := range plans {
		wrote, err := p.write()
		if err != nil {
			return changed, false, err
		}
		if wrote {
			changed = true
		}
	}
	return changed, false, nil
}

// valuesOf projects a present registry to its key/value map.
func valuesOf(present FileMap) map[string]string {
	m := make(map[string]string, len(present))
	for k, e := range present {
		m[k] = e.Value
	}
	return m
}

// tombstonesOf returns the set of keys the registry records as removed.
func tombstonesOf(reg FileMap) map[string]bool {
	t := make(map[string]bool)
	for k, e := range reg {
		if !e.Present() {
			t[k] = true
		}
	}
	return t
}

// atomicWrite writes data to a temp file named with prefix in path's directory,
// chmods it to mode, and renames it over path. A failed write leaves no temp behind.
func atomicWrite(path string, data []byte, mode os.FileMode, prefix string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), prefix)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp for %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("rename temp over %s: %w", path, err)
	}
	if err = syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync directory for %s: %w", path, err)
	}
	return nil
}

func syncDirectory(path string) (err error) {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	return dir.Sync()
}
