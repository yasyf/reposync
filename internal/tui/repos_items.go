package tui

import (
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/reposync/internal/discover"
)

// repoItem is one discovered repository row, carrying its candidate so an
// enabled selection can be applied without re-scanning. Live status, the dirty
// reason, and the precise last-activity time arrive asynchronously; mtime is the
// instant sort key computed at scan time.
type repoItem struct {
	cand     discover.Candidate
	selected bool
	status   repoStatus
	reason   string
	activity time.Time
	mtime    time.Time
}

func (i repoItem) FilterValue() string { return i.cand.Relpath }

// sortKey is the timestamp a row sorts by: its precise last-activity time when
// known, else the filesystem mtime captured at scan time.
func (i repoItem) sortKey() time.Time {
	if !i.activity.IsZero() {
		return i.activity
	}
	return i.mtime
}

func newRepoItems(cands []discover.Candidate) []list.Item {
	items := make([]list.Item, len(cands))
	for i, c := range cands {
		items[i] = repoItem{
			cand:     c,
			selected: c.Tracked,
			mtime:    repoMTime(c.AbsPath, c.Kind),
		}
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

	// The master column is narrow, so the row stays terse — relpath plus kind —
	// and the detail pane carries origin, trunk, and state. Truncate to the list
	// width so a long relpath never wraps and breaks the single-line layout.
	row := it.status.glyph() + " " + box + " " + it.cand.Relpath + " " + dim.Render("("+it.cand.Kind+")")

	if index == m.Index() {
		row = "> " + row
	} else {
		row = "  " + row
	}
	if it.selected != it.cand.Tracked {
		row = pendingAccent.Render(row)
	}

	_, _ = io.WriteString(w, lipgloss.NewStyle().MaxWidth(m.Width()).Render(row))
}

// renderRepoDetail describes the selected repo for the detail pane: its kind,
// origin, tracked-or-pending state, and checkout path. Live VCS status and last
// activity are grafted in once the status pipeline reports them.
func renderRepoDetail(item list.Item) string {
	it, ok := item.(repoItem)
	if !ok {
		return dim.Render("No repo selected.")
	}
	c := it.cand

	origin := c.Origin
	if c.LocalOnly {
		origin = "(local-only)"
	}

	state := badgeTracked.Render("tracked")
	if !c.Tracked {
		state = dim.Render("untracked")
	}
	if it.selected != c.Tracked {
		state = pendingAccent.Render("pending change")
	}

	status := it.status.label()
	if it.status != statusUnknown && it.reason != "" {
		status += dim.Render(" (" + it.reason + ")")
	}

	lines := []string{
		detailTitle.Render(c.Relpath),
		"",
		detailKey.Render("kind   ") + badgeKind.Render(c.Kind),
		detailKey.Render("origin ") + origin,
		detailKey.Render("state  ") + state,
		detailKey.Render("status ") + status,
		detailKey.Render("edited ") + relTime(it.sortKey()),
		detailKey.Render("path   ") + dim.Render(c.AbsPath),
	}
	return strings.Join(lines, "\n")
}
