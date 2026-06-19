package tui

import "github.com/charmbracelet/lipgloss"

var (
	activeTab   = lipgloss.NewStyle().Bold(true).Foreground(colActive).Padding(0, 1)
	inactiveTab = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	tabSep      = lipgloss.NewStyle().Faint(true)

	statusErr  = lipgloss.NewStyle().Foreground(colErr)
	statusOK   = lipgloss.NewStyle().Foreground(colOK)
	statusInfo = lipgloss.NewStyle().Faint(true)

	pendingAccent = lipgloss.NewStyle().Foreground(colWarn)
	badgeTracked  = lipgloss.NewStyle().Faint(true)
	dim           = lipgloss.NewStyle().Faint(true)

	glyphOK    = lipgloss.NewStyle().Foreground(colOK)
	glyphWarn  = lipgloss.NewStyle().Foreground(colWarn)
	glyphFail  = lipgloss.NewStyle().Foreground(colErr)
	glyphCheck = lipgloss.NewStyle().Foreground(colCheck)

	confirmBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colWarn).
			Padding(0, 1)

	logPane = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colBorder).
		Padding(0, 1)
)
