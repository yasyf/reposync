package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// screen is one tab of the TUI. Update returns the concrete screen so the
// router replaces it in place, keeping all per-screen state on the value.
type screen interface {
	Init() tea.Cmd
	Update(tea.Msg) (screen, tea.Cmd)
	View() string
	Title() string
	Help() []key.Binding
	// wantsKey reports whether a modal sub-state (a focused text input or an
	// open confirm dialog) should swallow a key before the router's globals run.
	wantsKey(tea.KeyMsg) bool
}
