package tui

import (
	"io"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/reposync/internal/discover"
)

// repoItem is one discovered repository row, carrying its candidate so an
// enabled selection can be applied without re-scanning.
type repoItem struct {
	cand     discover.Candidate
	selected bool
}

func (i repoItem) FilterValue() string { return i.cand.Relpath }

func newRepoItems(cands []discover.Candidate) []list.Item {
	items := make([]list.Item, len(cands))
	for i, c := range cands {
		items[i] = repoItem{cand: c, selected: c.Tracked}
	}
	return items
}

// repoDelegate renders a repoItem as a checkbox row, accenting any row whose
// selection diverges from its tracked state (a pending change).
type repoDelegate struct{}

func (repoDelegate) Height() int                         { return 1 }
func (repoDelegate) Spacing() int                        { return 0 }
func (repoDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d repoDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it := item.(repoItem)

	box := "[ ]"
	if it.selected {
		box = "[x]"
	}

	origin := it.cand.Origin
	if it.cand.LocalOnly {
		origin = "(local-only)"
	}

	row := box + " " + it.cand.Relpath + " (" + it.cand.Kind + ")"
	if it.cand.Tracked {
		row += " " + badgeTracked.Render("tracked")
	}
	row += " " + dim.Render(origin)

	if index == m.Index() {
		row = "> " + row
	} else {
		row = "  " + row
	}
	if it.selected != it.cand.Tracked {
		row = pendingAccent.Render(row)
	}

	io.WriteString(w, lipgloss.NewStyle().Render(row))
}
