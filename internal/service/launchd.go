// Package service installs reposync as a launchd user agent on macOS.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
	"time"
)

// Label is the launchd job label and reverse-DNS plist filename stem.
const Label = "com.github.yasyf.reposync"

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BinPath}}</string>
		<string>sync</string>
		<string>--config</string>
		<string>{{.ConfigPath}}</string>
	</array>
	<key>StartInterval</key>
	<integer>{{.IntervalSeconds}}</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
</dict>
</plist>
`))

type plistData struct {
	Label           string
	BinPath         string
	ConfigPath      string
	IntervalSeconds int
	LogPath         string
}

// PlistPath returns the LaunchAgent plist path for the current user.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

func logPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "reposync.log"), nil
}

// Install writes the launchd plist and loads it via launchctl.
func Install(configPath string, interval time.Duration) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("launchd install is only supported on macOS, not %s", runtime.GOOS)
	}

	binPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own path: %w", err)
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		return "", fmt.Errorf("resolve own path: %w", err)
	}

	plist, err := PlistPath()
	if err != nil {
		return "", err
	}
	logFile, err := logPath()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return "", err
	}

	f, err := os.Create(plist)
	if err != nil {
		return "", err
	}
	defer f.Close()

	data := plistData{
		Label:           Label,
		BinPath:         binPath,
		ConfigPath:      configPath,
		IntervalSeconds: int(interval.Seconds()),
		LogPath:         logFile,
	}
	if err := plistTemplate.Execute(f, data); err != nil {
		return "", fmt.Errorf("render plist: %w", err)
	}

	// Reload: unload any stale job first, ignoring "not loaded" errors.
	_ = exec.Command("launchctl", "unload", plist).Run()
	if err := exec.Command("launchctl", "load", plist).Run(); err != nil {
		return "", fmt.Errorf("launchctl load: %w", err)
	}

	return plist, nil
}

// Uninstall unloads the launchd job and removes the plist.
func Uninstall() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("launchd uninstall is only supported on macOS, not %s", runtime.GOOS)
	}

	plist, err := PlistPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return "", fmt.Errorf("not installed: %s does not exist", plist)
	}

	_ = exec.Command("launchctl", "unload", plist).Run()
	if err := os.Remove(plist); err != nil {
		return "", fmt.Errorf("remove plist: %w", err)
	}

	return plist, nil
}
