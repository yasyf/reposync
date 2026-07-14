package env

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/synckit/cregistry"
)

var (
	tOld = time.Unix(1_600_000_000, 0)
	tMid = time.Unix(1_700_000_000, 0)
	tNew = time.Unix(1_800_000_000, 0)
)

func micros(t time.Time) cregistry.Micros { return cregistry.UnixMicros(t) }

// chtimes pins path's mtime, failing the test on error.
func chtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// writeEnv writes an env file at root/name with mtime pinned to at.
func writeEnv(t *testing.T, root, name, content string, at time.Time) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	chtimes(t, path, at)
}

// regAt builds a single-key present registry stamped at.
func regAt(key, val string, at cregistry.Micros) FileMap {
	r := cregistry.New[string]()
	r.Add(key, val, at)
	return r
}

func TestScanNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".env", ".env.local", ".envrc", "config.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, ".env.d"), 0o750); err != nil {
		t.Fatalf("mkdir .env.d: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, ".env.link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	names, err := ScanNames(root)
	if err != nil {
		t.Fatalf("ScanNames: %v", err)
	}
	want := []string{".env", ".env.local"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("ScanNames = %v, want %v", names, want)
	}
}

func TestValidateFileName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{"../x", false},
		{"a/b", false},
		{".bashrc", false},
		{".envrc", false},
		{"sub/.env", false},
		{"", false},
		{".env.d/x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateFileName(c.name)
			if (err == nil) != c.ok {
				t.Errorf("ValidateFileName(%q) err = %v, want ok=%v", c.name, err, c.ok)
			}
		})
	}
}

func TestObserve(t *testing.T) {
	origin := "git@example.com:me/repo.git"
	cases := []struct {
		id     string
		files  map[string]string  // name -> content, written with mtime tNew
		before map[string]FileMap // sidecar prior state
		assert func(t *testing.T, out RepoState)
	}{
		{
			id:    "add new key",
			files: map[string]string{".env": "A=1\n"},
			assert: func(t *testing.T, out RepoState) {
				e := out[".env"]["A"]
				if !e.Present() || e.Value != "1" || e.Added != micros(tNew) {
					t.Errorf("A = %+v, want present value 1 at %d", e, micros(tNew))
				}
			},
		},
		{
			id:     "change existing key",
			files:  map[string]string{".env": "A=2\n"},
			before: map[string]FileMap{".env": regAt("A", "1", micros(tOld))},
			assert: func(t *testing.T, out RepoState) {
				e := out[".env"]["A"]
				if !e.Present() || e.Value != "2" || e.Added != micros(tNew) {
					t.Errorf("A = %+v, want present value 2 at %d", e, micros(tNew))
				}
			},
		},
		{
			id:    "delete key gone from file",
			files: map[string]string{".env": "B=9\n"},
			before: map[string]FileMap{".env": func() FileMap {
				r := regAt("A", "1", micros(tOld))
				r.Add("B", "9", micros(tOld))
				return r
			}()},
			assert: func(t *testing.T, out RepoState) {
				if out[".env"]["A"].Present() {
					t.Error("A still present, want tombstoned")
				}
				if out[".env"]["A"].Removed != micros(tNew) {
					t.Errorf("A.Removed = %d, want %d", out[".env"]["A"].Removed, micros(tNew))
				}
				if !out[".env"]["B"].Present() {
					t.Error("B not present")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range c.files {
				writeEnv(t, root, name, content, tNew)
			}
			sc := Sidecar{Origin: origin, Files: RepoState{}}
			for name, reg := range c.before {
				sc.Files[name] = reg
			}
			out, err := Observe(sc, root, []string{".env"})
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			c.assert(t, out)
		})
	}
}

// TestObserveIdempotent pins that persisting Observe's output and folding again is a
// no-op: the second fold is deeply equal to the first across add, change, and delete.
func TestObserveIdempotent(t *testing.T) {
	root := t.TempDir()
	writeEnv(t, root, ".env", "A=2\nC=3\n", tNew)
	before := regAt("A", "1", micros(tOld))
	before.Add("B", "keep", micros(tOld))
	sc := Sidecar{Origin: "o", Files: RepoState{".env": before}}

	first, err := Observe(sc, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe first: %v", err)
	}
	second, err := Observe(Sidecar{Origin: "o", Files: first}, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe second: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("second fold diverged:\n first = %#v\n second = %#v", first, second)
	}
	// Also confirm two folds of the same sidecar match (determinism).
	repeat, err := Observe(sc, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe repeat: %v", err)
	}
	if !reflect.DeepEqual(first, repeat) {
		t.Errorf("repeat fold of same sidecar diverged")
	}
}

// TestObserveMtimeRegression pins that a differing value whose mtime does not exceed
// the entry's stamp still takes, deterministically bumped to one past that stamp.
func TestObserveMtimeRegression(t *testing.T) {
	root := t.TempDir()
	writeEnv(t, root, ".env", "A=2\n", tOld)
	big := micros(tNew) // sidecar stamp newer than the file's old mtime
	sc := Sidecar{Origin: "o", Files: RepoState{".env": regAt("A", "1", big)}}
	out, err := Observe(sc, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	e := out[".env"]["A"]
	if e.Value != "2" || e.Added != big+1 {
		t.Errorf("A = %+v, want value 2 at %d", e, big+1)
	}
}

// TestObserveWholeFileDeletion pins that a synced file that vanished tombstones its
// present keys at the repo-root directory's mtime.
func TestObserveWholeFileDeletion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	chtimes(t, root, tNew)
	sc := Sidecar{Origin: "o", Files: RepoState{".env": regAt("A", "1", micros(tOld))}}
	out, err := Observe(sc, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	e := out[".env"]["A"]
	if e.Present() {
		t.Error("A still present, want tombstoned")
	}
	if e.Removed != micros(tNew) {
		t.Errorf("A.Removed = %d, want root dir mtime %d", e.Removed, micros(tNew))
	}
}

// TestObserveNoOpCases pins that a missing or empty file that was never synced folds
// to nothing.
func TestObserveNoOpCases(t *testing.T) {
	t.Run("missing never synced", func(t *testing.T) {
		root := t.TempDir()
		out, err := Observe(Sidecar{Origin: "o", Files: RepoState{}}, root, []string{".env"})
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("out = %v, want empty", out)
		}
	})
	t.Run("empty never synced", func(t *testing.T) {
		root := t.TempDir()
		writeEnv(t, root, ".env", "", tNew)
		out, err := Observe(Sidecar{Origin: "o", Files: RepoState{}}, root, []string{".env"})
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("out = %v, want empty", out)
		}
	})
}

// TestObserveOversizedSkipped pins that a file over MaxFileSize is skipped, leaving no
// entry in the fold.
func TestObserveOversizedSkipped(t *testing.T) {
	root := t.TempDir()
	big := append([]byte("A="), bytes.Repeat([]byte("x"), MaxFileSize)...)
	if err := os.WriteFile(filepath.Join(root, ".env"), big, 0o600); err != nil {
		t.Fatalf("write big: %v", err)
	}
	chtimes(t, filepath.Join(root, ".env"), tNew)
	out, err := Observe(Sidecar{Origin: "o", Files: RepoState{}}, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if _, ok := out[".env"]; ok {
		t.Errorf("out has .env, want oversized file skipped")
	}
}

// assertNoLeak fails if marker appears in any value across the fold output.
func assertNoLeak(t *testing.T, out RepoState, marker string) {
	t.Helper()
	for name, reg := range out {
		for k, e := range reg {
			if strings.Contains(e.Value, marker) {
				t.Errorf("marker %q leaked into %s[%q] = %q", marker, name, k, e.Value)
			}
		}
	}
}

// TestObserveExemptSymlink pins that a previously-synced name whose path is now a
// symlink is exempt: its sidecar keys are carried through unchanged (no tombstones)
// and the symlink target's content is never read into the fold.
func TestObserveExemptSymlink(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("SECRET=topsecretvalue\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(root, ".env")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	seeded := regAt("A", "1", micros(tOld))
	sc := Sidecar{Origin: "o", Files: RepoState{".env": seeded}}
	out, err := Observe(sc, root, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !reflect.DeepEqual(out[".env"], seeded) {
		t.Errorf("out[.env] = %#v, want sidecar carried through unchanged %#v", out[".env"], seeded)
	}
	if _, ok := out[".env"]["SECRET"]; ok {
		t.Error("symlink target key SECRET leaked into the fold")
	}
	assertNoLeak(t, out, "topsecretvalue")
}

// TestObserveExemptOversized pins that a previously-synced name now over MaxFileSize is
// exempt: its sidecar keys are carried through unchanged (no tombstones) and the file's
// new keys are not observed.
func TestObserveExemptOversized(t *testing.T) {
	root := t.TempDir()
	big := append([]byte("NEWKEY=leak\nA="), bytes.Repeat([]byte("y"), MaxFileSize)...)
	if err := os.WriteFile(filepath.Join(root, ".env"), big, 0o600); err != nil {
		t.Fatalf("write big: %v", err)
	}
	chtimes(t, filepath.Join(root, ".env"), tNew)
	seeded := regAt("A", "1", micros(tOld))
	sc := Sidecar{Origin: "o", Files: RepoState{".env": seeded}}
	out, err := Observe(sc, root, []string{".env"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !reflect.DeepEqual(out[".env"], seeded) {
		t.Errorf("out[.env] = %#v, want sidecar carried through unchanged %#v", out[".env"], seeded)
	}
	if _, ok := out[".env"]["NEWKEY"]; ok {
		t.Error("oversized file key NEWKEY was observed, want exempt")
	}
	if e := out[".env"]["A"]; !e.Present() || e.Value != "1" {
		t.Errorf("A = %+v, want unchanged present value 1", e)
	}
}

// TestExempt pins the exempt predicate across a regular small file (not exempt), a
// symlink and a directory (non-regular, exempt), an oversized regular file (exempt),
// and a missing path (not exempt).
func TestExempt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("write regular: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, ".env.link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".env.d"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := append([]byte("A="), bytes.Repeat([]byte("x"), MaxFileSize)...)
	if err := os.WriteFile(filepath.Join(root, ".env.big"), big, 0o600); err != nil {
		t.Fatalf("write big: %v", err)
	}
	cases := []struct {
		name string
		want bool
	}{
		{".env", false},
		{".env.link", true},
		{".env.d", true},
		{".env.big", true},
		{".env.missing", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Exempt(root, c.name)
			if err != nil {
				t.Fatalf("Exempt(%q): %v", c.name, err)
			}
			if got != c.want {
				t.Errorf("Exempt(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
