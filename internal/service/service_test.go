package service

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	synckit "github.com/yasyf/synckit/service"
)

func agentBySuffix(t *testing.T, suffix string) synckit.AgentSpec {
	t.Helper()
	for _, a := range config().Agents {
		if a.Label == suffix {
			return a
		}
	}
	t.Fatalf("no agent with label suffix %q", suffix)
	return synckit.AgentSpec{}
}

// TestConfigLabels pins the two full launchd labels exactly as deployed today;
// these strings appear in install.go output and any live plist, so they must not
// drift.
func TestConfigLabels(t *testing.T) {
	if TickLabel != "com.github.yasyf.reposync.reconcile" {
		t.Errorf("TickLabel = %q", TickLabel)
	}
	if WatchLabel != "com.github.yasyf.reposync.watch" {
		t.Errorf("WatchLabel = %q", WatchLabel)
	}
	cfg := config()
	if got := cfg.FullLabel(agentBySuffix(t, reconcileSuffix)); got != TickLabel {
		t.Errorf("reconcile full label = %q, want %q", got, TickLabel)
	}
	if got := cfg.FullLabel(agentBySuffix(t, watchSuffix)); got != WatchLabel {
		t.Errorf("watch full label = %q, want %q", got, WatchLabel)
	}
}

// TestConfigAgents pins reposync's per-agent verb and plist keys: the tick is a
// low-priority background reconcile on a 900s interval; the watch is kept alive.
// Cross-agent keys must not leak between them.
func TestConfigAgents(t *testing.T) {
	tick := agentBySuffix(t, reconcileSuffix)
	if tick.Command != "reconcile" {
		t.Errorf("tick command = %q", tick.Command)
	}
	for key, want := range map[string]any{
		"StartInterval":    900,
		"ThrottleInterval": 900,
		"RunAtLoad":        true,
		"ProcessType":      "Background",
		"Nice":             10,
		"LowPriorityIO":    true,
	} {
		if tick.ExtraKeys[key] != want {
			t.Errorf("tick[%q] = %v, want %v", key, tick.ExtraKeys[key], want)
		}
	}
	for _, absent := range []string{"KeepAlive", "LimitLoadToSessionType"} {
		if _, ok := tick.ExtraKeys[absent]; ok {
			t.Errorf("tick unexpectedly has key %q", absent)
		}
	}

	watch := agentBySuffix(t, watchSuffix)
	if watch.Command != "watch" {
		t.Errorf("watch command = %q", watch.Command)
	}
	for key, want := range map[string]any{
		"KeepAlive":        true,
		"RunAtLoad":        true,
		"ThrottleInterval": 10,
		"ProcessType":      "Background",
	} {
		if watch.ExtraKeys[key] != want {
			t.Errorf("watch[%q] = %v, want %v", key, watch.ExtraKeys[key], want)
		}
	}
	for _, absent := range []string{"StartInterval", "Nice", "LowPriorityIO"} {
		if _, ok := watch.ExtraKeys[absent]; ok {
			t.Errorf("watch unexpectedly has key %q", absent)
		}
	}
}

// TestLogName pins reposync's per-agent log file names; the generic layer derives
// log paths from the full label, and reposync keeps its historical filenames.
func TestLogName(t *testing.T) {
	if got := logName(TickLabel); got != "Library/Logs/reposync.log" {
		t.Errorf("logName(tick) = %q", got)
	}
	if got := logName(WatchLabel); got != "Library/Logs/reposync-watch.log" {
		t.Errorf("logName(watch) = %q", got)
	}
}

// parsePlist parses a launchd plist's top-level <dict> into a flat map, with nested
// <dict>/<array> as map/slice; it fails the test on malformed XML.
func parsePlist(t *testing.T, xmlStr string) map[string]any {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(xmlStr))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			t.Fatalf("plist has no top-level <dict>")
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "dict" {
			return parseDict(t, dec)
		}
	}
}

func parseDict(t *testing.T, dec *xml.Decoder) map[string]any {
	t.Helper()
	out := map[string]any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("plist dict parse: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local != "key" {
				t.Fatalf("expected <key>, got <%s>", el.Name.Local)
			}
			out[readChardata(t, dec)] = parseValue(t, dec)
		case xml.EndElement:
			if el.Name.Local == "dict" {
				return out
			}
		}
	}
}

func parseValue(t *testing.T, dec *xml.Decoder) any {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("plist value parse: %v", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "string":
			return readChardata(t, dec)
		case "integer":
			n, err := strconv.Atoi(strings.TrimSpace(readChardata(t, dec)))
			if err != nil {
				t.Fatalf("plist integer parse: %v", err)
			}
			return n
		case "true":
			return true
		case "false":
			return false
		case "dict":
			return parseDict(t, dec)
		case "array":
			return parseArray(t, dec)
		default:
			t.Fatalf("unexpected plist value <%s>", start.Name.Local)
		}
	}
}

func parseArray(t *testing.T, dec *xml.Decoder) []any {
	t.Helper()
	var out []any
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("plist array parse: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local == "string" {
				out = append(out, readChardata(t, dec))
			}
		case xml.EndElement:
			if el.Name.Local == "array" {
				return out
			}
		}
	}
}

func readChardata(t *testing.T, dec *xml.Decoder) string {
	t.Helper()
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("plist chardata parse: %v", err)
		}
		switch el := tok.(type) {
		case xml.CharData:
			sb.Write(el)
		case xml.EndElement:
			return sb.String()
		}
	}
}

// fakeLoader records the plist paths passed to Bootstrap and the labels passed to
// Bootout, in call order.
type fakeLoader struct {
	bootstrapped []string // plist paths
	bootedOut    []string // launchd labels
}

func (f *fakeLoader) Bootstrap(_ context.Context, plistPath string) error {
	f.bootstrapped = append(f.bootstrapped, plistPath)
	return nil
}

func (f *fakeLoader) Bootout(_ context.Context, label string) error {
	f.bootedOut = append(f.bootedOut, label)
	return nil
}

func skipNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skipf("Install/Uninstall are macOS-only; skipping on %s", runtime.GOOS)
	}
}

func plistPath(home, label string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func readPlist(t *testing.T, home, label string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(plistPath(home, label)) //nolint:gosec // G304: test reads a plist from a test-controlled temp home.
	if err != nil {
		t.Fatalf("read %s plist: %v", label, err)
	}
	return parsePlist(t, string(data))
}

func TestInstallBothAgents(t *testing.T) {
	skipNonDarwin(t)
	requireWatchman(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	l := &fakeLoader{}
	if err := Install(context.Background(), l, false); err != nil {
		t.Fatalf("Install: %v", err)
	}

	tickPath := plistPath(home, TickLabel)
	watchPath := plistPath(home, WatchLabel)

	tick := readPlist(t, home, TickLabel)
	if tick["Label"] != TickLabel || tick["StartInterval"] != 900 || tick["LowPriorityIO"] != true {
		t.Errorf("tick plist on disk wrong: %v", tick)
	}
	args, _ := tick["ProgramArguments"].([]any)
	if len(args) != 2 || args[1] != "reconcile" {
		t.Errorf("tick ProgramArguments = %v", tick["ProgramArguments"])
	}
	wantTickLog := filepath.Join(home, "Library", "Logs", "reposync.log")
	if tick["StandardOutPath"] != wantTickLog || tick["StandardErrorPath"] != wantTickLog {
		t.Errorf("tick log path = %v, want %q", tick["StandardOutPath"], wantTickLog)
	}

	watch := readPlist(t, home, WatchLabel)
	if watch["Label"] != WatchLabel || watch["KeepAlive"] != true {
		t.Errorf("watch plist on disk wrong: %v", watch)
	}
	wantWatchLog := filepath.Join(home, "Library", "Logs", "reposync-watch.log")
	if watch["StandardOutPath"] != wantWatchLog {
		t.Errorf("watch log path = %v, want %q", watch["StandardOutPath"], wantWatchLog)
	}

	if !equalStrings(l.bootstrapped, []string{tickPath, watchPath}) {
		t.Errorf("Bootstrap calls = %v, want %v", l.bootstrapped, []string{tickPath, watchPath})
	}
	// Each install boots out before bootstrap so reload picks up changes.
	if !equalStrings(l.bootedOut, []string{TickLabel, WatchLabel}) {
		t.Errorf("Bootout calls = %v, want %v", l.bootedOut, []string{TickLabel, WatchLabel})
	}
}

func TestInstallTickOnly(t *testing.T) {
	skipNonDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	l := &fakeLoader{}
	if err := Install(context.Background(), l, true); err != nil {
		t.Fatalf("Install: %v", err)
	}

	tickPath := plistPath(home, TickLabel)
	if _, err := os.Stat(tickPath); err != nil {
		t.Errorf("tick plist should exist: %v", err)
	}
	if _, err := os.Stat(plistPath(home, WatchLabel)); !os.IsNotExist(err) {
		t.Errorf("watch plist should be absent, got err=%v", err)
	}
	if !equalStrings(l.bootstrapped, []string{tickPath}) {
		t.Errorf("Bootstrap calls = %v, want %v", l.bootstrapped, []string{tickPath})
	}
}

func TestInstallPlistMode(t *testing.T) {
	skipNonDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Install(context.Background(), &fakeLoader{}, true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	info, err := os.Stat(plistPath(home, TickLabel))
	if err != nil {
		t.Fatalf("stat tick plist: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("tick plist mode = %o, want 644", info.Mode().Perm())
	}
}

func TestUninstall(t *testing.T) {
	skipNonDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Install(context.Background(), &fakeLoader{}, true); err != nil {
		t.Fatalf("Install: %v", err)
	}

	l := &fakeLoader{}
	if err := Uninstall(context.Background(), l); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if !equalStrings(l.bootedOut, []string{TickLabel, WatchLabel}) {
		t.Errorf("Bootout calls = %v, want %v", l.bootedOut, []string{TickLabel, WatchLabel})
	}
	for _, p := range []string{plistPath(home, TickLabel), plistPath(home, WatchLabel)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("plist %s should be removed, got err=%v", p, err)
		}
	}
}

func TestUninstallMissingFilesOK(t *testing.T) {
	skipNonDarwin(t)
	t.Setenv("HOME", t.TempDir())

	l := &fakeLoader{}
	if err := Uninstall(context.Background(), l); err != nil {
		t.Fatalf("Uninstall with no installed agents should succeed: %v", err)
	}
	if len(l.bootedOut) != 2 {
		t.Errorf("expected 2 Bootout calls, got %d", len(l.bootedOut))
	}
}

// requireWatchman skips a both-agents test when watchman is absent, since the watch
// agent's preflight makes it a hard install error by design.
func requireWatchman(t *testing.T) {
	t.Helper()
	if err := preflight(context.Background(), agentBySuffix(t, watchSuffix)); err != nil {
		t.Skipf("watch agent requires watchman: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		//nolint:gosec // G602: guarded above by len(a) != len(b), so b[i] is in range for every i in range a.
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
