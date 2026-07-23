package env

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/hostregistry"
)

// sidecarTempPrefix names a sidecar's temp file during an atomic save.
const sidecarTempPrefix = ".reposync-sidecar-"

// sanitizeMax caps the human-readable portion of a sidecar filename; the origin
// hash disambiguates truncated or collapsed names.
const sanitizeMax = 64

const (
	sidecarIdentity    = "repo-sync-env-sidecar-v1"
	sidecarVersion     = uint64(1)
	sidecarDeclaration = "schema:{identity:string,version:uint64,fingerprint:string};origin:string;files:map<string,map<string,{added_at:int64,removed_at?:int64,value:string}>>"
)

var sidecarFingerprint = hostregistry.SchemaFingerprint(sidecarIdentity, sidecarDeclaration)

type sidecarSchema struct {
	Identity    string `json:"identity"`
	Version     uint64 `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

type sidecarEnvelope struct {
	Schema sidecarSchema `json:"schema"`
	Origin string        `json:"origin"`
	Files  RepoState     `json:"files"`
}

type exactMicros struct {
	value cregistry.Micros
	set   bool
}

func (value *exactMicros) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("timestamp must not be null")
	}
	if err := json.Unmarshal(data, &value.value); err != nil {
		return err
	}
	value.set = true
	return nil
}

type exactString struct {
	value string
	set   bool
}

func (value *exactString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("value must not be null")
	}
	if err := json.Unmarshal(data, &value.value); err != nil {
		return err
	}
	value.set = true
	return nil
}

type sidecarEntryJSON struct {
	Added   exactMicros `json:"added_at"`
	Removed exactMicros `json:"removed_at"`
	Value   exactString `json:"value"`
}

type sidecarEnvelopeJSON struct {
	Schema sidecarSchema                          `json:"schema"`
	Origin string                                 `json:"origin"`
	Files  map[string]map[string]sidecarEntryJSON `json:"files"`
}

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
	if origin == "" {
		return Sidecar{}, errors.New("sidecar origin is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Sidecar{Origin: origin, Files: make(RepoState)}, nil
		}
		return Sidecar{}, fmt.Errorf("stat sidecar %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Sidecar{}, fmt.Errorf("sidecar %s must be a regular 0600 file", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: sidecar under reposync's own config dir, not user-supplied.
	if err != nil {
		return Sidecar{}, fmt.Errorf("read sidecar %s: %w", path, err)
	}
	sc, err := decodeSidecar(data)
	if err != nil {
		return Sidecar{}, fmt.Errorf("parse sidecar %s: %w", path, err)
	}
	if sc.Origin != origin {
		return Sidecar{}, fmt.Errorf("sidecar %s: origin %q does not match %q", path, sc.Origin, origin)
	}
	return sc, nil
}

func decodeSidecar(data []byte) (Sidecar, error) {
	var persisted sidecarEnvelopeJSON
	if err := hostregistry.DecodeExactJSON(data, &persisted); err != nil {
		return Sidecar{}, err
	}
	if persisted.Schema.Identity != sidecarIdentity ||
		persisted.Schema.Version != sidecarVersion ||
		persisted.Schema.Fingerprint != sidecarFingerprint {
		return Sidecar{}, errors.New("sidecar schema does not match exact v1")
	}
	if persisted.Origin == "" || persisted.Files == nil {
		return Sidecar{}, errors.New("sidecar requires origin and files")
	}
	files := make(RepoState, len(persisted.Files))
	for name, entries := range persisted.Files {
		if len(entries) == 0 {
			return Sidecar{}, fmt.Errorf("sidecar file %q is empty or null", name)
		}
		reg := make(FileMap, len(entries))
		for key, entry := range entries {
			if !entry.Added.set || !entry.Value.set {
				return Sidecar{}, fmt.Errorf("sidecar entry %q in %q is missing fields", key, name)
			}
			reg[key] = cregistry.Entry[string]{
				Added: entry.Added.value, Removed: entry.Removed.value, Value: entry.Value.value,
			}
		}
		files[name] = reg
	}
	sc := Sidecar{Origin: persisted.Origin, Files: files}
	if err := validateSidecar(sc); err != nil {
		return Sidecar{}, err
	}
	return sc, nil
}

// Save GCs expired tombstones, blanks the value of every surviving tombstone so no
// deleted secret is persisted, drops emptied files, and atomically writes the sidecar
// (dir 0700, file 0600). A tombstone is dropped once it is absent and its newest stamp
// is older than TombstoneTTL.
func (sc Sidecar) Save(path string) error {
	if err := validateSidecar(sc); err != nil {
		return err
	}
	cutoff := cregistry.UnixMicros(Now().Add(-TombstoneTTL))
	files := make(RepoState, len(sc.Files))
	for name, reg := range sc.Files {
		kept := make(FileMap, len(reg))
		for k, e := range reg {
			if !e.Present() {
				if maxStamp(e) < cutoff {
					continue
				}
				e.Value = ""
			}
			kept[k] = e
		}
		if len(kept) == 0 {
			continue
		}
		files[name] = kept
	}
	persisted := sidecarEnvelope{
		Schema: sidecarSchema{
			Identity: sidecarIdentity, Version: sidecarVersion, Fingerprint: sidecarFingerprint,
		},
		Origin: sc.Origin,
		Files:  files,
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create sidecar dir %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat sidecar dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sidecar dir %s is not a directory", dir)
	}
	// #nosec G302 -- directories require execute permission; 0700 is owner-only.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure sidecar dir %s: %w", dir, err)
	}
	return atomicWrite(path, data, 0o600, sidecarTempPrefix)
}

func validateSidecar(sc Sidecar) error {
	if sc.Origin == "" || sc.Files == nil {
		return errors.New("sidecar requires origin and files")
	}
	for name, reg := range sc.Files {
		if err := ValidateFileName(name); err != nil {
			return err
		}
		if reg == nil {
			return fmt.Errorf("sidecar file %q is null", name)
		}
		for key, entry := range reg {
			if !ValidKey(key) || !ValidValue(entry.Value) ||
				entry.Added < 0 || entry.Removed < 0 || maxStamp(entry) <= 0 {
				return fmt.Errorf("sidecar entry %q in %q is invalid", key, name)
			}
		}
	}
	return nil
}

// maxStamp returns the newer of an entry's add and remove stamps.
func maxStamp(e cregistry.Entry[string]) cregistry.Micros {
	if e.Removed > e.Added {
		return e.Removed
	}
	return e.Added
}
