package env

import (
	"testing"

	"github.com/yasyf/synckit/cregistry"
)

func TestMergeLWW(t *testing.T) {
	older := RepoState{".env": regAt("A", "old", micros(tOld))}
	newer := RepoState{".env": regAt("A", "new", micros(tNew))}

	for _, order := range []struct {
		id     string
		states []RepoState
	}{
		{"older then newer", []RepoState{older, newer}},
		{"newer then older", []RepoState{newer, older}},
	} {
		t.Run(order.id, func(t *testing.T) {
			out := Merge(order.states...)
			e := out[".env"]["A"]
			if e.Value != "new" || e.Added != micros(tNew) {
				t.Errorf("A = %+v, want value new at %d", e, micros(tNew))
			}
		})
	}
}

// TestMergeEditVsDeleteTie pins the cregistry tie rule: an edit and a delete stamped
// identically resolve to the delete.
func TestMergeEditVsDeleteTie(t *testing.T) {
	const base, conflict = cregistry.Micros(1000), cregistry.Micros(2000)
	edit := cregistry.New[string]()
	edit.Add("A", "edited", conflict)

	del := cregistry.New[string]()
	del.Add("A", "base", base)
	del.Remove("A", conflict)

	out := Merge(RepoState{".env": edit}, RepoState{".env": del})
	if out[".env"]["A"].Present() {
		t.Errorf("A = %+v, want tombstoned (delete wins the tie)", out[".env"]["A"])
	}
}

func TestDigest(t *testing.T) {
	base := RepoState{".env": regAt("A", "v", micros(tOld))}

	stampDiff := RepoState{".env": regAt("A", "v", micros(tNew))}
	if Digest(base) != Digest(stampDiff) {
		t.Error("digest changed across a stamp-only difference, want stable")
	}

	withTombstone := RepoState{".env": func() FileMap {
		r := regAt("A", "v", micros(tOld))
		r.Add("B", "gone", micros(tOld))
		r.Remove("B", micros(tNew))
		return r
	}()}
	if Digest(base) != Digest(withTombstone) {
		t.Error("digest changed when a tombstone was added, want stable")
	}

	valueDiff := RepoState{".env": regAt("A", "w", micros(tOld))}
	if Digest(base) == Digest(valueDiff) {
		t.Error("digest unchanged across a value change, want different")
	}
}
