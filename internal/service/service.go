// Package service manages the two macOS LaunchAgents that drive reposync: a
// periodic tick that runs reconcile and a long-lived watch daemon. Plist
// generation is a set of pure functions so tests assert the exact XML; the
// launchctl boundary is injected so tests never load real agents.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
)

const (
	// TickLabel is the launchd label for the periodic reconcile tick.
	TickLabel = "com.github.yasyf.reposync.reconcile"
	// WatchLabel is the launchd label for the long-lived watch daemon.
	WatchLabel = "com.github.yasyf.reposync.watch"

	tickLogRelpath  = "Library/Logs/reposync.log"
	watchLogRelpath = "Library/Logs/reposync-watch.log"

	launchAgentsRelpath = "Library/LaunchAgents"

	// daemonPath is the PATH the LaunchAgents run with. launchd's default PATH
	// omits the Homebrew prefixes where jj and watchman live, so reconcile and the
	// watch daemon fail to find them; EnvironmentVariables replaces the process
	// PATH outright, so the system dirs are kept too. Both arches are listed so a
	// single plist works on Apple Silicon and Intel.
	daemonPath = "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/local/sbin:/usr/bin:/bin:/usr/sbin:/sbin"

	tickInterval  = 900
	tickNice      = 10
	watchThrottle = 10
	plistFileMode = 0o644
	agentsDirMode = 0o755

	// notLoadedExit is launchctl bootout's exit code (ESRCH) when the target agent
	// isn't loaded — the only tolerated bootout failure, by code, never by stderr text.
	notLoadedExit = 3
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
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
	</dict>
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
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
	</dict>
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

// Loader bootstraps and boots out launchd jobs in the user's GUI domain; the
// launchctl boundary tests inject. Bootout tolerates a not-loaded agent so a
// reinstall is idempotent; Bootstrap tolerates nothing, since install boots out
// first and a nonzero bootstrap (e.g. a malformed plist) is a real error.
type Loader interface {
	// Bootstrap registers the job described by the plist at plistPath.
	Bootstrap(ctx context.Context, plistPath string) error
	// Bootout deregisters the launchd job with the given label.
	Bootout(ctx context.Context, label string) error
}

// launchctlLoader is the production Loader: it shells out to launchctl's modern
// domain API (bootstrap/bootout gui/<uid>), which reports failures via exit code.
type launchctlLoader struct{}

// NewLauncher returns the default Loader backed by the launchctl CLI.
func NewLauncher() Loader {
	return launchctlLoader{}
}

func guiDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func (launchctlLoader) Bootstrap(ctx context.Context, plistPath string) error {
	//nolint:gosec // G204: plistPath is reposync's own generated launchd plist path, not user-supplied.
	cmd := exec.CommandContext(ctx, "launchctl", "bootstrap", guiDomain(), plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", plistPath, err, bytes.TrimSpace(out))
	}
	return nil
}

func (launchctlLoader) Bootout(ctx context.Context, label string) error {
	out, err := exec.CommandContext(ctx, "launchctl", "bootout", guiDomain()+"/"+label).CombinedOutput()
	if err == nil {
		return nil
	}
	// launchctl bootout exits 3 (ESRCH) when the agent isn't loaded — expected on a
	// first install and during reload. Tolerate that exit code only, by code not text.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == notLoadedExit {
		return nil
	}
	return fmt.Errorf("launchctl bootout %s: %w: %s", label, err, bytes.TrimSpace(out))
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
		"Path":     daemonPath,
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
		"Path":     daemonPath,
		"Throttle": watchThrottle,
		"LogPath":  logPath,
	})
}

// Install resolves this executable, writes the tick plist and bootstraps it, and
// unless tickOnly does the same for the watch plist. Each agent is booted out
// before bootstrap so re-install picks up changes.
func Install(ctx context.Context, l Loader, tickOnly bool) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("install requires macOS launchd, not %s", runtime.GOOS)
	}
	exe, err := exePath()
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

// Uninstall boots out both LaunchAgents and removes their plist files; a missing
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
		if err := l.Bootout(ctx, label); err != nil {
			return fmt.Errorf("bootout %s: %w", label, err)
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
	if err := l.Bootout(ctx, label); err != nil {
		return fmt.Errorf("bootout %s before reload: %w", label, err)
	}
	if err := l.Bootstrap(ctx, path); err != nil {
		return fmt.Errorf("bootstrap %s: %w", label, err)
	}
	return nil
}

// exePath returns the path used to invoke this binary, deliberately NOT
// resolving symlinks. On a Homebrew install that keeps the plist pointed at the
// stable /opt/homebrew/bin/reposync symlink, which brew relinks on every
// upgrade, so the LaunchAgents survive `brew upgrade` untouched. Resolving would
// bake in a versioned Caskroom path that the next upgrade purges, leaving the
// agents pointing at a deleted binary.
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	return exe, nil
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
