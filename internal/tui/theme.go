package tui

import "github.com/charmbracelet/lipgloss"

// The reposync palette. 256-color ANSI indices, one semantic role each.
const (
	colActive = lipgloss.Color("212") // pink — active tab
	colAccent = lipgloss.Color("37")  // teal — brand accent
	colOK     = lipgloss.Color("78")  // green — clean / ready
	colWarn   = lipgloss.Color("214") // orange — pending / reachable-not-installed
	colErr    = lipgloss.Color("203") // red — dirty / error / unreachable
	colCheck  = lipgloss.Color("39")  // blue — in-progress / checking
	colBorder = lipgloss.Color("240") // grey — idle panel border
)

var (
	accent = lipgloss.NewStyle().Foreground(colAccent)

	// panel and panelActive box a master or detail column; the active pane
	// borrows the accent border, idle panes stay grey.
	panel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colBorder).
		Padding(0, 1)
	panelActive = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colAccent).
			Padding(0, 1)

	// headerTitle is the brand mark in the top band; headerHint is the
	// right-aligned context (host identity, sort order).
	headerTitle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	headerHint  = lipgloss.NewStyle().Faint(true)

	// detailTitle heads a detail pane; detailKey labels a field.
	detailTitle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	detailKey   = lipgloss.NewStyle().Faint(true)

	// Row and detail status badges.
	badgeClean = lipgloss.NewStyle().Foreground(colOK)
	badgeDirty = lipgloss.NewStyle().Foreground(colWarn)
	badgeSync  = lipgloss.NewStyle().Foreground(colCheck)
	badgeKind  = accent
)
