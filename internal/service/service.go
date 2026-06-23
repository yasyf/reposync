// Package service manages the two macOS LaunchAgents that drive reposync: a
// periodic tick that runs reconcile and a long-lived watch daemon. The generic
// launchd/launchctl machinery — deterministic plist rendering, the launchctl
// boundary, install/uninstall ordering — lives in the public
// github.com/yasyf/synckit/service package; this package supplies reposync's
// ToolConfig (labels, verbs, schedule, the watchman preflight) and delegates to it.
package service

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/yasyf/synckit/service"
)

const (
	// TickLabel is the launchd label for the periodic reconcile tick.
	TickLabel = labelPrefix + "." + reconcileSuffix
	// WatchLabel is the launchd label for the long-lived watch daemon.
	WatchLabel = labelPrefix + "." + watchSuffix

	labelPrefix     = "com.github.yasyf.reposync"
	reconcileSuffix = "reconcile"
	watchSuffix     = "watch"

	tickLogRelpath  = "Library/Logs/reposync.log"
	watchLogRelpath = "Library/Logs/reposync-watch.log"

	tickInterval  = 900
	tickNice      = 10
	watchThrottle = 10
)

// Loader bootstraps and boots out launchd jobs; the launchctl boundary tests inject.
type Loader = service.Launcher

// NewLauncher returns the default Loader backed by the launchctl CLI.
func NewLauncher() Loader {
	return service.NewLauncher()
}

// config is reposync's launchd job set: the reconcile tick (low-priority background,
// every tickInterval seconds) and the watch daemon (kept alive, watchman required).
// Per-agent log files keep reposync's existing names. The watchman preflight guards
// the watch agent only.
func config() service.ToolConfig {
	return service.ToolConfig{
		BinaryName:     "reposync",
		LabelPrefix:    labelPrefix,
		DaemonPATH:     service.DefaultDaemonPATH,
		LogName:        logName,
		PreflightCheck: preflight,
		Agents: []service.AgentSpec{
			{Label: reconcileSuffix, Command: "reconcile", ExtraKeys: map[string]any{
				"StartInterval":    tickInterval,
				"ThrottleInterval": tickInterval,
				"RunAtLoad":        true,
				"ProcessType":      "Background",
				"Nice":             tickNice,
				"LowPriorityIO":    true,
			}},
			{Label: watchSuffix, Command: "watch", ExtraKeys: map[string]any{
				"KeepAlive":        true,
				"RunAtLoad":        true,
				"ThrottleInterval": watchThrottle,
				"ProcessType":      "Background",
			}},
		},
	}
}

func logName(label string) string {
	switch label {
	case WatchLabel:
		return watchLogRelpath
	default:
		return tickLogRelpath
	}
}

// preflight requires watchman before the watch agent loads; the tick agent has no
// external dependency. watchman provides the file-watch backend the watch daemon
// drives, so its absence is a hard install error (pass --tick-only to skip it).
func preflight(_ context.Context, agent service.AgentSpec) error {
	if agent.Label != watchSuffix {
		return nil
	}
	if _, err := exec.LookPath("watchman"); err != nil {
		return fmt.Errorf("watchman is required by the watch daemon but was not found (install it or pass --tick-only): %w", err)
	}
	return nil
}

// Install writes the reconcile tick plist and bootstraps it, and unless tickOnly
// does the same for the watch plist. Each agent is booted out before bootstrap so
// re-install picks up changes; the watch agent requires watchman.
func Install(ctx context.Context, l Loader, tickOnly bool) error {
	return service.NewLaunchdManager(l).Install(ctx, config(), tickOnly)
}

// Uninstall boots out both LaunchAgents and removes their plist files; a missing
// file is not an error.
func Uninstall(ctx context.Context, l Loader) error {
	return service.NewLaunchdManager(l).Uninstall(ctx, config())
}
