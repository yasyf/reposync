package env

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/synckit/cregistry"
)

// present builds a registry whose keys are all present, stamping each add distinctly.
func present(kv map[string]string) FileMap {
	r := cregistry.New[string]()
	at := cregistry.Micros(1000)
	for k, v := range kv {
		r.Add(k, v, at)
		at++
	}
	return r
}

// tombstone builds a registry with each key removed after an add, so all are absent.
func tombstone(keys ...string) FileMap {
	r := cregistry.New[string]()
	for _, k := range keys {
		r.Add(k, "gone", 1000)
		r.Remove(k, 2000)
	}
	return r
}

func TestParseRoundTrip(t *testing.T) {
	cases := []struct {
		id   string
		in   string
		vals map[string]string
	}{
		{"empty", "", map[string]string{}},
		{"trailing newline", "A=1\n", map[string]string{"A": "1"}},
		{"no trailing newline", "A=1", map[string]string{"A": "1"}},
		{"export prefix", "export FOO=bar\n", map[string]string{"FOO": "bar"}},
		{"export double space", "export  FOO=bar\n", map[string]string{"FOO": "bar"}},
		{"quotes verbatim", "A=\"hello world\"\n", map[string]string{"A": "\"hello world\""}},
		{"equals in value", "URL=https://example.com/p?a=1&b=2\n", map[string]string{"URL": "https://example.com/p?a=1&b=2"}},
		{"space before equals", "FOO =bar\n", map[string]string{"FOO": "bar"}},
		{"comment preserved", "# a comment\nA=1\n", map[string]string{"A": "1"}},
		{"blank lines preserved", "\n\nA=1\n\n", map[string]string{"A": "1"}},
		{"malformed leading digit", "1BAD=x\nA=1\n", map[string]string{"A": "1"}},
		{"malformed no left side", "=x\nA=1\n", map[string]string{"A": "1"}},
		{"unterminated quote is a raw value", "A=\"line1\nline2\"\n", map[string]string{"A": "\"line1"}},
		{"duplicate last wins", "A=1\nA=2\n", map[string]string{"A": "2"}},
		{"crlf stays in value", "A=1\r\nB=2\n", map[string]string{"A": "1\r", "B": "2"}},
		{"export no space is a key", "exportFOO=bar\n", map[string]string{"exportFOO": "bar"}},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			f := parse([]byte(c.in))
			got := f.values()
			if len(got) != len(c.vals) {
				t.Fatalf("values() = %v, want %v", got, c.vals)
			}
			for k, v := range c.vals {
				if got[k] != v {
					t.Errorf("values()[%q] = %q, want %q", k, got[k], v)
				}
			}
			if round := string(f.rewrite(f.values(), nil)); round != c.in {
				t.Errorf("round-trip = %q, want %q", round, c.in)
			}
		})
	}
}

func TestApply(t *testing.T) {
	cases := []struct {
		id      string
		initial string // "" with create=false means the file is absent
		create  bool
		reg     FileMap
		want    string // expected file content after apply
		exists  bool   // whether the file exists after apply
		changed bool
	}{
		{
			id: "update last occurrence in place", initial: "A=1\nA=2\n", create: true,
			reg:  present(map[string]string{"A": "9"}),
			want: "A=1\nA=9\n", exists: true, changed: true,
		},
		{
			id: "no write when identical", initial: "A=1\n", create: true,
			reg:  present(map[string]string{"A": "1"}),
			want: "A=1\n", exists: true, changed: false,
		},
		{
			id: "tombstone drops all occurrences", initial: "A=1\n# keep\nA=2\nB=3\n", create: true,
			reg:  tombstone("A"),
			want: "# keep\nB=3\n", exists: true, changed: true,
		},
		{
			id: "sorted append at eof", initial: "# header\n", create: true,
			reg:  present(map[string]string{"ZED": "z", "ABE": "a", "MID": "m"}),
			want: "# header\nABE=a\nMID=m\nZED=z\n", exists: true, changed: true,
		},
		{
			id: "never delete file when all tombstoned", initial: "A=1\n", create: true,
			reg:  tombstone("A"),
			want: "", exists: true, changed: true,
		},
		{
			id: "create when nonempty", create: false,
			reg:  present(map[string]string{"B": "2", "A": "1"}),
			want: "A=1\nB=2\n", exists: true, changed: true,
		},
		{
			id: "no create when only tombstones", create: false,
			reg:  tombstone("A"),
			want: "", exists: false, changed: false,
		},
		{
			id: "layout only key untouched", initial: "LOCAL=keepme\n", create: true,
			reg:  present(map[string]string{"NEW": "v"}),
			want: "LOCAL=keepme\nNEW=v\n", exists: true, changed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			if c.create {
				if err := os.WriteFile(path, []byte(c.initial), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			changed, err := Apply(path, c.reg)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if changed != c.changed {
				t.Errorf("changed = %v, want %v", changed, c.changed)
			}
			_, statErr := os.Stat(path)
			if got := statErr == nil; got != c.exists {
				t.Fatalf("file exists = %v, want %v", got, c.exists)
			}
			if !c.exists {
				return
			}
			data, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file from a test-controlled temp dir.
			if err != nil {
				t.Fatalf("read result: %v", err)
			}
			if string(data) != c.want {
				t.Errorf("content = %q, want %q", data, c.want)
			}
		})
	}
}

// TestApplyPreservesMode pins that a rewrite keeps the existing file's permission bits.
func TestApplyPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil { //nolint:gosec // G302: test asserts perm preservation of a group-readable file.
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Apply(path, present(map[string]string{"A": "2"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o640))
	}
}

// TestApplyCreatesMode0600 pins that a created env file is 0600.
func TestApplyCreatesMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if _, err := Apply(path, present(map[string]string{"A": "1"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o600))
	}
}

// TestApplyLeavesNoTempAndTempPrefixNeverScans pins that a rewrite leaves no temp
// file behind and that the temp prefix is invisible to the scanner.
func TestApplyLeavesNoTempAndTempPrefixNeverScans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Apply(path, present(map[string]string{"A": "2"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if matchesPattern(tempPrefix + "abc123") {
		t.Errorf("temp name %q matches the scan pattern", tempPrefix+"abc123")
	}
	if err := os.WriteFile(filepath.Join(dir, tempPrefix+"leftover"), nil, 0o600); err != nil {
		t.Fatalf("write temp-shaped file: %v", err)
	}
	names, err := ScanNames(dir)
	if err != nil {
		t.Fatalf("ScanNames: %v", err)
	}
	if len(names) != 1 || names[0] != ".env" {
		t.Errorf("ScanNames = %v, want [.env] (temp-prefixed file ignored)", names)
	}
}

// TestApplySkipsSymlink pins that Apply leaves an exempt symlink target untouched: no
// write, the path stays a symlink to the same target, the target's bytes are unchanged.
func TestApplySkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("SECRET=keep\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	changed, err := Apply(link, present(map[string]string{"A": "1"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changed {
		t.Error("Apply reported a write to an exempt symlink")
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("path is no longer a symlink: mode %v", info.Mode())
	}
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != target {
		t.Errorf("symlink dest = %q, want %q", dest, target)
	}
	data, err := os.ReadFile(target) //nolint:gosec // G304: test reads a test-controlled temp file.
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "SECRET=keep\n" {
		t.Errorf("target content = %q, want unchanged", data)
	}
}

// TestApplySkipsOversized pins that Apply leaves a target over MaxFileSize untouched.
func TestApplySkipsOversized(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	original := append([]byte("A="), bytes.Repeat([]byte("z"), MaxFileSize)...)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changed, err := Apply(path, present(map[string]string{"A": "1"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changed {
		t.Error("Apply reported a write to an exempt oversized file")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: test reads a test-controlled temp file.
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, original) {
		t.Errorf("oversized file was modified: len %d, want %d", len(data), len(original))
	}
}
