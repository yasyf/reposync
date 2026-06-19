package tui

import "github.com/charmbracelet/lipgloss"

const (
	panelHChrome = 4  // rounded border (2) + horizontal padding (2)
	panelVChrome = 2  // rounded border top + bottom
	mdMinWidth   = 64 // below this the detail column is hidden
	mdMinDetail  = 24 // hide the detail column rather than render it cramped
	mdListBoxMin = 30 // floor for the master column so rows stay legible
)

// splitDims divides a screen area into a master (list) column and an optional
// detail column, returning the inner content sizes each panel should hold. The
// returned height already excludes the panels' border rows; callers size their
// list with (listW, height) and box the result with masterDetail.
func splitDims(width, height int) (listW, detailW, contentH int, showDetail bool) {
	contentH = height - panelVChrome
	if contentH < 1 {
		contentH = 1
	}

	if width < mdMinWidth {
		return max(1, width-panelHChrome), 0, contentH, false
	}

	listBox := width * 2 / 5
	if listBox < mdListBoxMin {
		listBox = mdListBoxMin
	}
	detailW = width - listBox - panelHChrome
	if detailW < mdMinDetail {
		return max(1, width-panelHChrome), 0, contentH, false
	}
	return listBox - panelHChrome, detailW, contentH, true
}

// masterDetail boxes a sized list view beside a detail string. The master pane
// carries the accent border (it always holds focus today); the detail pane stays
// grey. When the screen is too narrow, the list is boxed on its own.
func masterDetail(listView, detail string, listW, detailW, height int, showDetail bool) string {
	left := panelActive.Width(listW).Height(height).Render(listView)
	if !showDetail {
		return left
	}
	right := panel.Width(detailW).Height(height).Render(detail)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}
