package tui

import (
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tabBarLines and helpLines are the fixed chrome rows the router reserves above
// and below the active screen when laying out its inner height.
const (
	tabBarLines = 1
	helpLines   = 1
)

// rootModel is the tab router over the repos and hosts screens.
type rootModel struct {
	opts     Options
	active   int
	screens  [2]screen
	inited   [2]bool
	width    int
	height   int
	keys     globalKeyMap
	help     help.Model
	quitting bool
}

func newRootModel(opts Options) rootModel {
	return rootModel{
		opts:    opts,
		screens: [2]screen{newReposModel(opts), newHostsModel(opts)},
		inited:  [2]bool{true, false},
		keys:    newGlobalKeyMap(),
		help:    help.New(),
	}
}

func (m rootModel) Init() tea.Cmd {
	return m.screens[0].Init()
}

func (m rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		inner := tea.WindowSizeMsg{Width: msg.Width, Height: m.innerHeight()}
		var cmds []tea.Cmd
		for i := range m.screens {
			s, cmd := m.screens[i].Update(inner)
			m.screens[i] = s
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		if m.screens[m.active].wantsKey(msg) {
			s, cmd := m.screens[m.active].Update(msg)
			m.screens[m.active] = s
			return m, cmd
		}
		switch {
		case key.Matches(msg, m.keys.Quit):
			m.quitting = true
			return m, tea.Quit
		case key.Matches(msg, m.keys.NextTab):
			m.active = (m.active + 1) % len(m.screens)
			if !m.inited[m.active] {
				m.inited[m.active] = true
				return m, m.screens[m.active].Init()
			}
			return m, nil
		case key.Matches(msg, m.keys.Help):
			m.help.ShowAll = !m.help.ShowAll
			return m, nil
		}
		s, cmd := m.screens[m.active].Update(msg)
		m.screens[m.active] = s
		return m, cmd

	default:
		var cmds []tea.Cmd
		for i := range m.screens {
			s, cmd := m.screens[i].Update(msg)
			m.screens[i] = s
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
}

func (m rootModel) View() string {
	if m.quitting {
		return ""
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.tabBar(), m.screens[m.active].View(), m.helpView())
}

func (m rootModel) innerHeight() int {
	inner := m.height - tabBarLines - helpLines
	if inner < 1 {
		return 1
	}
	return inner
}

func (m rootModel) tabBar() string {
	tabs := make([]string, len(m.screens))
	for i, s := range m.screens {
		if i == m.active {
			tabs[i] = activeTab.Render(s.Title())
			continue
		}
		tabs[i] = inactiveTab.Render(s.Title())
	}
	return tabSep.Render("[ ") + tabs[0] + tabSep.Render(" | ") + tabs[1] + tabSep.Render(" ]")
}

func (m rootModel) helpView() string {
	bindings := append(m.screens[m.active].Help(), m.keys.NextTab, m.keys.Help, m.keys.Quit)
	if m.help.ShowAll {
		return m.help.FullHelpView([][]key.Binding{bindings})
	}
	return m.help.ShortHelpView(bindings)
}
