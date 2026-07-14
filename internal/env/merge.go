package env

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/yasyf/synckit/cregistry"
)

// Merge joins the per-file registries of states into one RepoState, taking the
// cregistry lattice join per file name.
func Merge(states ...RepoState) RepoState {
	out := make(RepoState)
	for _, s := range states {
		for name, reg := range s {
			out[name] = cregistry.Merge(out[name], reg)
		}
	}
	return out
}

// Digest is a stable fingerprint of a RepoState's present content: the sha256 of the
// canonical JSON of file name to present key to value. It excludes stamps,
// tombstones, and layout, so it is unchanged by an apply that preserves values —
// this apply-stability is what the daemon's echo suppression relies on.
func Digest(rs RepoState) string {
	canonical := make(map[string]map[string]string, len(rs))
	for name, reg := range rs {
		present := reg.Present()
		if len(present) == 0 {
			continue
		}
		m := make(map[string]string, len(present))
		for k, e := range present {
			m[k] = e.Value
		}
		canonical[name] = m
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		panic("env: RepoState digest is not JSON-serializable: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
