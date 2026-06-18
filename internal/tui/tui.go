// Package tui is the interactive terminal UI launched by bare `reposync` on a
// TTY: two discover-toggle-apply screens for enabling repos and managing hosts.
package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yasyf/reposync/internal/host"
)

// Options configures a TUI run.
type Options struct {
	Version string
	Runner  host.Runner
}

// Run launches the interactive TUI and blocks until the user quits or ctx is
// canceled. A ctx-driven teardown (ctrl-c, SIGTERM) is a clean exit.
func Run(ctx context.Context, opts Options) error {
	p := tea.NewProgram(newRootModel(opts), tea.WithContext(ctx), tea.WithAltScreen())
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
