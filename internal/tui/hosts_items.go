package tui

import (
	"io"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/reposync/internal/host"
)

// verifyState tracks how far a host's reachability probe has progressed.
type verifyState int

const (
	verifyUnknown verifyState = iota
	verifyChecking
	verifyOK
	verifyWarn
	verifyFail
)

// hostItem is one host row: a discovered or registered peer plus the latest
// probe result.
type hostItem struct {
	node       string
	target     string
	source     string
	online     bool
	registered bool
	verify     host.VerifyResult
	state      verifyState
}

func (i hostItem) FilterValue() string { return i.target }

// hostDelegate renders a hostItem: a verify glyph, the target, a registration
// marker, and a source/online hint.
type hostDelegate struct{}

func (hostDelegate) Height() int                         { return 1 }
func (hostDelegate) Spacing() int                        { return 0 }
func (hostDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d hostDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it := item.(hostItem)

	glyph := dim.Render("·")
	switch it.state {
	case verifyChecking:
		glyph = glyphCheck.Render("…")
	case verifyOK:
		glyph = glyphOK.Render("✓")
	case verifyWarn:
		glyph = glyphWarn.Render("⚠")
	case verifyFail:
		glyph = glyphFail.Render("✗")
	}

	mark := dim.Render("[ ]")
	if it.registered {
		mark = badgeTracked.Render("[reg]")
	}

	hint := it.source
	if it.online {
		hint += " online"
	}
	if it.state == verifyOK && it.verify.Version != "" {
		hint += " " + it.verify.Version
	}
	if it.state == verifyWarn {
		hint += " not-installed"
	}
	if it.state == verifyFail && it.verify.Err != nil {
		hint += " unreachable"
	}

	row := glyph + " " + mark + " " + it.target + "  " + dim.Render(hint)
	if index == m.Index() {
		row = "> " + row
	} else {
		row = "  " + row
	}
	io.WriteString(w, lipgloss.NewStyle().Render(row))
}

// classifyVerify maps a probe result onto a row state.
func classifyVerify(res host.VerifyResult) verifyState {
	if res.Reachable && res.Bootstrapped {
		return verifyOK
	}
	if res.Reachable {
		return verifyWarn
	}
	return verifyFail
}
