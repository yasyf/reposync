package tui

import (
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/synckit/hostregistry"
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
	verify     hostregistry.VerifyResult
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
	_, _ = io.WriteString(w, lipgloss.NewStyle().MaxWidth(m.Width()).Render(row))
}

// renderHostDetail describes the selected host for the detail pane: its node,
// discovery source, online and registration state, and the latest probe result.
func renderHostDetail(item list.Item) string {
	it, ok := item.(hostItem)
	if !ok {
		return dim.Render("No host selected.")
	}

	reg := dim.Render("unregistered")
	if it.registered {
		reg = badgeTracked.Render("registered")
	}

	online := dim.Render("offline")
	if it.online {
		online = badgeClean.Render("online")
	}

	status := dim.Render("· not checked")
	switch it.state {
	case verifyChecking:
		status = glyphCheck.Render("… checking")
	case verifyOK:
		status = glyphOK.Render("✓ ready")
	case verifyWarn:
		status = glyphWarn.Render("⚠ reachable, not installed")
	case verifyFail:
		status = glyphFail.Render("✗ unreachable")
	}

	lines := []string{
		detailTitle.Render(it.target),
		"",
		detailKey.Render("node    ") + it.node,
		detailKey.Render("source  ") + it.source,
		detailKey.Render("online  ") + online,
		detailKey.Render("reg     ") + reg,
		detailKey.Render("status  ") + status,
	}
	if it.state == verifyOK && it.verify.Version != "" {
		lines = append(lines, detailKey.Render("version ")+it.verify.Version)
	}
	if it.state == verifyFail && it.verify.Err != nil {
		lines = append(lines, "", dim.Render(it.verify.Err.Error()))
	}
	return strings.Join(lines, "\n")
}

// classifyVerify maps a probe result onto a row state.
func classifyVerify(res hostregistry.VerifyResult) verifyState {
	if res.Reachable && res.Bootstrapped {
		return verifyOK
	}
	if res.Reachable {
		return verifyWarn
	}
	return verifyFail
}
