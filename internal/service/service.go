// Package service manages the two macOS LaunchAgents that drive reposync: a
// periodic tick that runs reconcile and a long-lived watch daemon. Plist
// generation is a set of pure functions so tests assert the exact XML; the
// launchctl boundary is injected so tests never load real agents.
package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
)

const (
	// TickLabel is the launchd label for the periodic reconcile tick.
	TickLabel = "com.github.yasyf.reposync"
	// WatchLabel is the launchd label for the long-lived watch daemon.
	WatchLabel = "com.github.yasyf.reposync.watch"

	tickLogRelpath  = "Library/Logs/reposync.log"
	watchLogRelpath = "Library/Logs/reposync-watch.log"

	launchAgentsRelpath = "Library/LaunchAgents"

	tickInterval  = 900
	tickNice      = 10
	watchThrottle = 10
	plistFileMode = 0o644
	agentsDirMode = 0o755
)

var tickTemplate = template.Must(template.New("tick").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Exe}}</string>
		<string>reconcile</string>
	</array>
	<key>StartInterval</key>
	<integer>{{.Interval}}</integer>
	<key>ThrottleInterval</key>
	<integer>{{.Interval}}</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>Nice</key>
	<integer>{{.Nice}}</integer>
	<key>LowPriorityIO</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
</dict>
</plist>
`))

var watchTemplate = template.Must(template.New("watch").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Exe}}</string>
		<string>watch</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>{{.Throttle}}</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
</dict>
</plist>
`))

// Loader loads and unloads launchd jobs; the launchctl boundary tests inject.
type Loader interface {
	// Load registers the job described by the plist at plistPath with launchd.
	Load(ctx context.Context, plistPath string) error
	// Unload deregisters the job described by the plist at plistPath.
	Unload(ctx context.Context, plistPath string) error
}

// launchctlLoader is the production Loader: it shells out to launchctl. Unload
// ignores the failure of a job that is not currently loaded so reload is safe.
type launchctlLoader struct{}

// NewLauncher returns the default Loader backed by the launchctl CLI.
func NewLauncher() Loader {
	return launchctlLoader{}
}

func (launchctlLoader) Load(ctx context.Context, plistPath string) error {
	cmd := exec.CommandContext(ctx, "launchctl", "load", plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load %s: %w: %s", plistPath, err, bytes.TrimSpace(out))
	}
	return nil
}

func (launchctlLoader) Unload(ctx context.Context, plistPath string) error {
	out, err := exec.CommandContext(ctx, "launchctl", "unload", plistPath).CombinedOutput()
	if err == nil {
		return nil
	}
	// launchctl unload exits non-zero when the job is not currently loaded; that
	// is expected on first install and during reload, so tolerate only that case.
	if bytes.Contains(out, []byte("Could not find specified service")) || bytes.Contains(out, []byte("no such process")) {
		return nil
	}
	return fmt.Errorf("launchctl unload %s: %w: %s", plistPath, err, bytes.TrimSpace(out))
}

// tickPlist renders the periodic reconcile tick's plist XML for the given
// executable path. It is OS-independent so tests run on any platform.
func tickPlist(exe string) (string, error) {
	logPath, err := homeJoin(tickLogRelpath)
	if err != nil {
		return "", err
	}
	return render(tickTemplate, map[string]any{
		"Label":    TickLabel,
		"Exe":      exe,
		"Interval": tickInterval,
		"Nice":     tickNice,
		"LogPath":  logPath,
	})
}

// watchPlist renders the watch daemon's plist XML for the given executable
// path. It is OS-independent so tests run on any platform.
func watchPlist(exe string) (string, error) {
	logPath, err := homeJoin(watchLogRelpath)
	if err != nil {
		return "", err
	}
	return render(watchTemplate, map[string]any{
		"Label":    WatchLabel,
		"Exe":      exe,
		"Throttle": watchThrottle,
		"LogPath":  logPath,
	})
}

// Install resolves this executable, writes the tick plist and loads it, and
// unless tickOnly does the same for the watch plist. Each plist is unloaded
// before loading so re-install picks up changes.
func Install(ctx context.Context, l Loader, tickOnly bool) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("install requires macOS launchd, not %s", runtime.GOOS)
	}
	exe, err := resolveExe()
	if err != nil {
		return err
	}
	agentsDir, err := homeJoin(launchAgentsRelpath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(agentsDir, agentsDirMode); err != nil {
		return fmt.Errorf("create LaunchAgents dir %s: %w", agentsDir, err)
	}

	if err := writeAndLoad(ctx, l, agentsDir, TickLabel, func() (string, error) { return tickPlist(exe) }); err != nil {
		return err
	}
	if tickOnly {
		return nil
	}
	if _, err := exec.LookPath("watchman"); err != nil {
		return fmt.Errorf("watchman is required by the watch daemon but was not found (install it or pass --tick-only): %w", err)
	}
	return writeAndLoad(ctx, l, agentsDir, WatchLabel, func() (string, error) { return watchPlist(exe) })
}

// Uninstall unloads both LaunchAgents and removes their plist files; a missing
// file is not an error.
func Uninstall(ctx context.Context, l Loader) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("uninstall requires macOS launchd, not %s", runtime.GOOS)
	}
	agentsDir, err := homeJoin(launchAgentsRelpath)
	if err != nil {
		return err
	}
	for _, label := range []string{TickLabel, WatchLabel} {
		path := filepath.Join(agentsDir, label+".plist")
		if err := l.Unload(ctx, path); err != nil {
			return fmt.Errorf("unload %s: %w", label, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove plist %s: %w", path, err)
		}
	}
	return nil
}

func writeAndLoad(ctx context.Context, l Loader, agentsDir, label string, render func() (string, error)) error {
	xml, err := render()
	if err != nil {
		return err
	}
	path := filepath.Join(agentsDir, label+".plist")
	if err := os.WriteFile(path, []byte(xml), plistFileMode); err != nil {
		return fmt.Errorf("write plist %s: %w", path, err)
	}
	if err := l.Unload(ctx, path); err != nil {
		return fmt.Errorf("unload %s before reload: %w", label, err)
	}
	if err := l.Load(ctx, path); err != nil {
		return fmt.Errorf("load %s: %w", label, err)
	}
	return nil
}

// resolveExe returns the absolute, symlink-resolved path to this binary so a
// Homebrew symlink in the plist points at the real binary.
func resolveExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve executable symlinks for %s: %w", exe, err)
	}
	return resolved, nil
}

func homeJoin(relpath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, relpath), nil
}

func render(t *template.Template, data map[string]any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render plist: %w", err)
	}
	return buf.String(), nil
}
