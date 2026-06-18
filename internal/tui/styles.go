package tui

import "github.com/charmbracelet/lipgloss"

var (
	activeTab   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Padding(0, 1)
	inactiveTab = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	tabSep      = lipgloss.NewStyle().Faint(true)

	statusErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	statusOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	statusInfo = lipgloss.NewStyle().Faint(true)

	pendingAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	badgeTracked  = lipgloss.NewStyle().Faint(true)
	dim           = lipgloss.NewStyle().Faint(true)

	glyphOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	glyphWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	glyphFail  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	glyphCheck = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))

	confirmBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("214")).
			Padding(0, 1)

	logPane = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1)
)
