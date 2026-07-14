package env

import (
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
