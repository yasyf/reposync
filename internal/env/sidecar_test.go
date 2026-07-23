package env

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yasyf/synckit/cregistry"
)

// pinNow fixes env.Now to at for the duration of the test.
func pinNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := Now
	Now = func() time.Time { return at }
	t.Cleanup(func() { Now = prev })
}

func TestSidecarRoundTrip(t *testing.T) {
	pinNow(t, tNew)
	origin := "git@example.com:me/repo.git"
	path := SidecarPath(t.TempDir(), origin)

	files := RepoState{".env": func() FileMap {
		r := regAt("A", "1", micros(tOld))
		r.Add("B", "two", micros(tMid))
		return r
	}()}
	sc := Sidecar{Origin: origin, Files: files}
	if err := sc.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadSidecar(path, origin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Origin != origin {
		t.Errorf("origin = %q, want %q", loaded.Origin, origin)
	}
	if !reflect.DeepEqual(loaded.Files, files) {
		t.Errorf("round-trip files = %#v, want %#v", loaded.Files, files)
	}
}

func TestSidecarPerms(t *testing.T) {
	pinNow(t, tNew)
	origin := "o"
	path := SidecarPath(t.TempDir(), origin)
	// #nosec G301 -- deliberately create a permissive fixture to verify tightening.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- deliberately weaken the fixture to verify tightening.
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sc := Sidecar{Origin: origin, Files: RepoState{".env": regAt("A", "1", micros(tOld))}}
	if err := sc.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %v, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %v, want 0700", got)
	}
}

func TestLoadSidecarRejectsNonExactV1(t *testing.T) {
	path := SidecarPath(t.TempDir(), "o")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := func(schema, origin, files string) string {
		return fmt.Sprintf(`{"schema":%s,"origin":%s,"files":%s}`+"\n", schema, origin, files)
	}
	schema := fmt.Sprintf(
		`{"identity":%q,"version":%d,"fingerprint":%q}`,
		sidecarIdentity, sidecarVersion, sidecarFingerprint,
	)
	entry := `{".env":{"A":{"added_at":1,"value":"secret"}}}`
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "foreign identity", raw: valid(`{"identity":"foreign","version":1,"fingerprint":"`+sidecarFingerprint+`"}`, `"o"`, entry)},
		{name: "foreign version", raw: valid(`{"identity":"`+sidecarIdentity+`","version":2,"fingerprint":"`+sidecarFingerprint+`"}`, `"o"`, entry)},
		{name: "fingerprint drift", raw: valid(`{"identity":"`+sidecarIdentity+`","version":1,"fingerprint":"drift"}`, `"o"`, entry)},
		{name: "corrupt", raw: `{"schema":`},
		{name: "trailing", raw: valid(schema, `"o"`, entry) + `{}`},
		{name: "duplicate", raw: `{"schema":` + schema + `,"origin":"o","origin":"o","files":` + entry + `}`},
		{name: "unknown top", raw: `{"schema":` + schema + `,"origin":"o","files":` + entry + `,"extra":1}`},
		{name: "unknown schema", raw: valid(`{"identity":"`+sidecarIdentity+`","version":1,"fingerprint":"`+sidecarFingerprint+`","extra":1}`, `"o"`, entry)},
		{name: "unknown entry", raw: valid(schema, `"o"`, `{".env":{"A":{"added_at":1,"value":"secret","extra":1}}}`)},
		{name: "missing schema", raw: `{"origin":"o","files":` + entry + `}`},
		{name: "missing origin", raw: `{"schema":` + schema + `,"files":` + entry + `}`},
		{name: "missing files", raw: `{"schema":` + schema + `,"origin":"o"}`},
		{name: "missing added", raw: valid(schema, `"o"`, `{".env":{"A":{"value":"secret"}}}`)},
		{name: "missing value", raw: valid(schema, `"o"`, `{".env":{"A":{"added_at":1}}}`)},
		{name: "null schema", raw: valid(`null`, `"o"`, entry)},
		{name: "null files", raw: valid(schema, `"o"`, `null`)},
		{name: "null file", raw: valid(schema, `"o"`, `{".env":null}`)},
		{name: "null added", raw: valid(schema, `"o"`, `{".env":{"A":{"added_at":null,"value":"secret"}}}`)},
		{name: "null removed", raw: valid(schema, `"o"`, `{".env":{"A":{"added_at":1,"removed_at":null,"value":"secret"}}}`)},
		{name: "null value", raw: valid(schema, `"o"`, `{".env":{"A":{"added_at":1,"value":null}}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSidecar(path, "o"); err == nil {
				t.Fatal("LoadSidecar accepted non-exact state")
			}
		})
	}
}

func TestSidecarSaveRejectsNilState(t *testing.T) {
	path := SidecarPath(t.TempDir(), "o")
	for _, sc := range []Sidecar{
		{Origin: "o"},
		{Origin: "o", Files: RepoState{".env": nil}},
	} {
		if err := sc.Save(path); err == nil {
			t.Fatalf("Save accepted nil state: %+v", sc)
		}
	}
}

func TestSidecarRoundTripPreservesThreeWayBase(t *testing.T) {
	pinNow(t, tNew)
	origin := "o"
	base := RepoState{".env": func() FileMap {
		reg := regAt("A", "base", micros(tOld))
		reg.Add("B", "delete-me", micros(tOld))
		return reg
	}()}
	path := SidecarPath(t.TempDir(), origin)
	if err := (Sidecar{Origin: origin, Files: base}).Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSidecar(path, origin)
	if err != nil {
		t.Fatal(err)
	}
	local := Merge(loaded.Files)
	local[".env"].Add("A", "local", micros(tNew))
	peer := Merge(loaded.Files)
	removedAt := micros(tNew.Add(-time.Hour))
	peer[".env"].Remove("B", removedAt)
	forward := Merge(local, peer)
	reverse := Merge(peer, local)
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("merge order diverged: forward=%+v reverse=%+v", forward, reverse)
	}
	if err := (Sidecar{Origin: origin, Files: forward}).Save(path); err != nil {
		t.Fatal(err)
	}
	converged, err := LoadSidecar(path, origin)
	if err != nil {
		t.Fatal(err)
	}
	if got := converged.Files[".env"]["A"].Value; got != "local" {
		t.Fatalf("A = %q, want local", got)
	}
	removed := converged.Files[".env"]["B"]
	if removed.Present() || removed.Removed != removedAt || removed.Value != "" {
		t.Fatalf("B = %+v, want confidential tombstone with preserved stamp", removed)
	}
	if Digest(converged.Files) != Digest(forward) ||
		Digest(Merge(converged.Files, reverse)) != Digest(forward) {
		t.Fatal("persisted base changed convergent present state")
	}
}

// TestLoadSidecarMissing pins that an absent sidecar loads as an empty state.
func TestLoadSidecarMissing(t *testing.T) {
	sc, err := LoadSidecar(SidecarPath(t.TempDir(), "o"), "o")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sc.Origin != "o" || len(sc.Files) != 0 {
		t.Errorf("sc = %+v, want empty state for origin o", sc)
	}
}

// TestLoadSidecarOriginMismatch pins that a sidecar whose embedded origin disagrees
// with the requested origin is a loud error.
func TestLoadSidecarOriginMismatch(t *testing.T) {
	pinNow(t, tNew)
	path := SidecarPath(t.TempDir(), "wanted")
	sc := Sidecar{Origin: "other", Files: RepoState{".env": regAt("A", "1", micros(tOld))}}
	if err := sc.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := LoadSidecar(path, "wanted"); err == nil {
		t.Error("LoadSidecar with mismatched origin succeeded, want error")
	}
}

func TestLoadSidecarRejectsInsecureFile(t *testing.T) {
	pinNow(t, tNew)
	origin := "o"
	path := SidecarPath(t.TempDir(), origin)
	if err := (Sidecar{
		Origin: origin, Files: RepoState{".env": regAt("SECRET", "value", micros(tOld))},
	}).Save(path); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- deliberately weaken the fixture to verify fail-closed loading.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSidecar(path, origin); err == nil {
		t.Fatal("LoadSidecar accepted a world-readable secret sidecar")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSidecar(path, origin); err == nil {
		t.Fatal("LoadSidecar followed a sidecar symlink")
	}
}

// TestSidecarSaveGC pins that Save drops expired tombstones and emptied files while
// keeping present keys and fresh tombstones.
func TestSidecarSaveGC(t *testing.T) {
	pinNow(t, tNew)
	origin := "o"
	path := SidecarPath(t.TempDir(), origin)

	fresh := micros(tNew.Add(-time.Hour))
	expired := micros(tNew.Add(-TombstoneTTL - 24*time.Hour))

	kept := cregistry.New[string]()
	kept.Add("P", "present", micros(tOld))
	kept.Add("F", "fresh", 1)
	kept.Remove("F", fresh)
	kept.Add("E", "expired", 1)
	kept.Remove("E", expired)

	gone := cregistry.New[string]()
	gone.Add("E2", "expired", 1)
	gone.Remove("E2", expired)

	sc := Sidecar{Origin: origin, Files: RepoState{".env": kept, ".env.gone": gone}}
	if err := sc.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadSidecar(path, origin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := loaded.Files[".env.gone"]; ok {
		t.Error(".env.gone survived, want emptied file dropped")
	}
	reg := loaded.Files[".env"]
	if _, ok := reg["E"]; ok {
		t.Error("expired tombstone E survived, want dropped")
	}
	if !reg["P"].Present() {
		t.Error("present key P dropped, want kept")
	}
	if reg["P"].Value != "present" {
		t.Errorf("present key P value = %q, want retained", reg["P"].Value)
	}
	if _, ok := reg["F"]; !ok || reg["F"].Present() {
		t.Errorf("fresh tombstone F = %+v, want kept and absent", reg["F"])
	}
	if reg["F"].Value != "" {
		t.Errorf("surviving tombstone F value = %q, want blanked in the persisted sidecar", reg["F"].Value)
	}
}
