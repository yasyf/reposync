package service

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeExe = "/opt/homebrew/Cellar/reposync/1.2.3/bin/reposync"

func assertWellFormed(t *testing.T, xmlStr string) {
	t.Helper()
	var anything struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(xmlStr), &anything); err != nil {
		t.Fatalf("plist is not well-formed XML: %v", err)
	}
}

func assertContains(t *testing.T, xmlStr string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(xmlStr, want) {
			t.Errorf("plist missing %q\n--- plist ---\n%s", want, xmlStr)
		}
	}
}

func assertNotContains(t *testing.T, xmlStr string, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(xmlStr, bad) {
			t.Errorf("plist unexpectedly contains %q", bad)
		}
	}
}

func TestTickPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := tickPlist(fakeExe)
	if err != nil {
		t.Fatalf("tickPlist: %v", err)
	}
	assertWellFormed(t, out)
	logPath := filepath.Join(home, tickLogRelpath)
	assertContains(t,
		out,
		"<string>com.github.yasyf.reposync.reconcile</string>",
		"<string>"+fakeExe+"</string>",
		"<string>reconcile</string>",
		"<key>EnvironmentVariables</key>",
		"<key>PATH</key>\n\t\t<string>"+daemonPath+"</string>",
		"<key>StartInterval</key>\n\t<integer>900</integer>",
		"<key>ThrottleInterval</key>\n\t<integer>900</integer>",
		"<key>Nice</key>\n\t<integer>10</integer>",
		"<key>LowPriorityIO</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>ProcessType</key>\n\t<string>Background</string>",
		"<key>StandardOutPath</key>\n\t<string>"+logPath+"</string>",
		"<key>StandardErrorPath</key>\n\t<string>"+logPath+"</string>",
	)
	assertNotContains(t, out, "<string>watch</string>", "<key>KeepAlive</key>")
}

func TestWatchPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := watchPlist(fakeExe)
	if err != nil {
		t.Fatalf("watchPlist: %v", err)
	}
	assertWellFormed(t, out)
	logPath := filepath.Join(home, watchLogRelpath)
	assertContains(t,
		out,
		"<string>com.github.yasyf.reposync.watch</string>",
		"<string>"+fakeExe+"</string>",
		"<string>watch</string>",
		"<key>EnvironmentVariables</key>",
		"<key>PATH</key>\n\t\t<string>"+daemonPath+"</string>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>ThrottleInterval</key>\n\t<integer>10</integer>",
		"<key>ProcessType</key>\n\t<string>Background</string>",
		"<key>StandardOutPath</key>\n\t<string>"+logPath+"</string>",
		"<key>StandardErrorPath</key>\n\t<string>"+logPath+"</string>",
	)
	assertNotContains(t,
		out,
		"<string>reconcile</string>",
		"<key>StartInterval</key>",
		"<key>Nice</key>",
		"<key>LowPriorityIO</key>",
	)
}

func TestPlistLogPathsAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, tc := range []struct {
		name    string
		render  func(string) (string, error)
		logName string
	}{
		{"tick", tickPlist, "reposync.log"},
		{"watch", watchPlist, "reposync-watch.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.render(fakeExe)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			want := "<string>" + filepath.Join(home, "Library", "Logs", tc.logName) + "</string>"
			if !strings.Contains(out, want) {
				t.Errorf("expected absolute log path %q in plist", want)
			}
			if strings.Contains(out, "~/Library") {
				t.Errorf("plist contains unexpanded ~ path")
			}
		})
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
	return filepath.Join(home, launchAgentsRelpath, label+".plist")
}

func TestInstallBothAgents(t *testing.T) {
	skipNonDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	l := &fakeLoader{}
	if err := Install(context.Background(), l, false); err != nil {
		t.Fatalf("Install: %v", err)
	}

	tickPath := plistPath(home, TickLabel)
	watchPath := plistPath(home, WatchLabel)

	tickData, err := os.ReadFile(tickPath) //nolint:gosec // G304: test reads a plist from a test-controlled temp home.
	if err != nil {
		t.Fatalf("read tick plist: %v", err)
	}
	assertContains(t, string(tickData),
		"<string>com.github.yasyf.reposync.reconcile</string>",
		"<string>reconcile</string>",
		"<key>StartInterval</key>\n\t<integer>900</integer>",
	)

	watchData, err := os.ReadFile(watchPath) //nolint:gosec // G304: test reads a plist from a test-controlled temp home.
	if err != nil {
		t.Fatalf("read watch plist: %v", err)
	}
	assertContains(t, string(watchData),
		"<string>com.github.yasyf.reposync.watch</string>",
		"<string>watch</string>",
		"<key>KeepAlive</key>\n\t<true/>",
	)

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
	watchPath := plistPath(home, WatchLabel)

	if _, err := os.Stat(tickPath); err != nil {
		t.Errorf("tick plist should exist: %v", err)
	}
	if _, err := os.Stat(watchPath); !os.IsNotExist(err) {
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
	if info.Mode().Perm() != plistFileMode {
		t.Errorf("tick plist mode = %o, want %o", info.Mode().Perm(), plistFileMode)
	}
}

func TestUninstall(t *testing.T) {
	skipNonDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Install(context.Background(), &fakeLoader{}, false); err != nil {
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
	home := t.TempDir()
	t.Setenv("HOME", home)

	l := &fakeLoader{}
	if err := Uninstall(context.Background(), l); err != nil {
		t.Fatalf("Uninstall with no installed agents should succeed: %v", err)
	}
	if len(l.bootedOut) != 2 {
		t.Errorf("expected 2 Bootout calls, got %d", len(l.bootedOut))
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
