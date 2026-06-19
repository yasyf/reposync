package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// filterBarLines is the single chrome row each screen reserves for its filter.
const filterBarLines = 1

// filterBar is an always-visible filter input owning its own state, independent
// of the bubbles list's built-in filter. '/' focuses it; esc clears and blurs.
type filterBar struct {
	input   textinput.Model
	focused bool
}

func newFilterBar() filterBar {
	in := textinput.New()
	in.Placeholder = "filter"
	in.Prompt = ""
	in.Width = 24
	return filterBar{input: in}
}

func (f filterBar) Value() string { return f.input.Value() }
func (f filterBar) Focused() bool { return f.focused }

func (f *filterBar) Focus() tea.Cmd { f.focused = true; return f.input.Focus() }
func (f *filterBar) Blur()          { f.focused = false; f.input.Blur() }
func (f *filterBar) Clear()         { f.input.SetValue("") }

func (f filterBar) Update(msg tea.Msg) (filterBar, tea.Cmd) {
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return f, cmd
}

// View renders the prompt, the input, and a live "N/M shown" match count.
func (f filterBar) View(visible, total int) string {
	count := dim.Render(fmt.Sprintf("  %d/%d", visible, total))
	return accent.Render("/ ") + f.input.View() + count
}

// filterItems narrows items to those whose FilterValue contains query, case-
// insensitively. It always returns a fresh slice so the caller may sort the
// result without disturbing the canonical slice; an empty query keeps every item.
func filterItems(all []list.Item, query string) []list.Item {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]list.Item, 0, len(all))
	for _, it := range all {
		if q == "" || strings.Contains(strings.ToLower(it.FilterValue()), q) {
			out = append(out, it)
		}
	}
	return out
}
