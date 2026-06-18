package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/reposync/internal/apply"
	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

// confirmState is an open yes/no dialog awaiting the selection it guards.
type confirmState struct {
	prompt string
	sel    apply.RepoSelection
}

type reposModel struct {
	opts     Options
	list     list.Model
	loading  bool
	applying bool
	spin     spinner.Model
	confirm  *confirmState
	status   string
	empty    bool
	scanDir  string
	keys     reposKeyMap
}

func newReposModel(opts Options) reposModel {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	l := list.New(nil, repoDelegate{}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	return reposModel{opts: opts, list: l, loading: true, spin: sp, keys: newReposKeyMap()}
}

func (m reposModel) Title() string { return "Repos" }

func (m reposModel) Help() []key.Binding {
	if m.confirm != nil {
		return []key.Binding{m.keys.Yes, m.keys.No}
	}
	return []key.Binding{m.keys.Toggle, m.keys.Apply}
}

func (m reposModel) wantsKey(tea.KeyMsg) bool {
	return m.confirm != nil || m.list.SettingFilter()
}

func (m reposModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, discoverReposCmd())
}

func (m reposModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height)
		return m, nil

	case reposLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = statusErr.Render(msg.err.Error())
			return m, nil
		}
		m.empty = len(msg.result.Candidates) == 0
		m.scanDir = scanDir()
		return m, m.list.SetItems(newRepoItems(msg.result.Candidates))

	case reposAppliedMsg:
		m.applying = false
		m.status = applySummary(msg.results, msg.err)
		return m, discoverReposCmd()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.loading || m.applying {
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m reposModel) handleKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	if m.confirm != nil {
		switch {
		case key.Matches(msg, m.keys.Yes):
			sel := m.confirm.sel
			m.confirm = nil
			m.applying = true
			return m, tea.Batch(m.spin.Tick, applyReposCmd(m.opts.Runner, sel))
		case key.Matches(msg, m.keys.No):
			m.confirm = nil
			return m, nil
		}
		return m, nil
	}

	if m.list.SettingFilter() {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	switch {
	case key.Matches(msg, m.keys.Toggle):
		return m.toggle()
	case key.Matches(msg, m.keys.Apply):
		return m.apply()
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m reposModel) toggle() (screen, tea.Cmd) {
	it, ok := m.list.SelectedItem().(repoItem)
	if !ok {
		return m, nil
	}
	it.selected = !it.selected
	return m, m.list.SetItem(m.list.GlobalIndex(), it)
}

func (m reposModel) apply() (screen, tea.Cmd) {
	var sel apply.RepoSelection
	for _, raw := range m.list.Items() {
		it := raw.(repoItem)
		switch {
		case it.selected && !it.cand.Tracked:
			sel.Enable = append(sel.Enable, it.cand)
		case it.cand.Tracked && !it.selected:
			sel.Disable = append(sel.Disable, it.cand.Relpath)
		}
	}
	if len(sel.Enable) == 0 && len(sel.Disable) == 0 {
		m.status = statusInfo.Render("nothing to apply")
		return m, nil
	}
	if len(sel.Disable) > 0 {
		m.confirm = &confirmState{
			prompt: fmt.Sprintf("Disable %d tracked repo(s)? They stop syncing. (y/N)", len(sel.Disable)),
			sel:    sel,
		}
		return m, nil
	}
	m.applying = true
	return m, tea.Batch(m.spin.Tick, applyReposCmd(m.opts.Runner, sel))
}

func (m reposModel) View() string {
	if m.loading {
		return m.spin.View() + " Scanning…"
	}
	if m.empty {
		return dim.Render(fmt.Sprintf("No git/jj repos found under %s.", m.scanDir))
	}

	body := m.list.View()
	if m.confirm != nil {
		body = lipgloss.JoinVertical(lipgloss.Left, body, confirmBox.Render(m.confirm.prompt))
	}
	if m.applying {
		body = lipgloss.JoinVertical(lipgloss.Left, body, m.spin.View()+" Applying…")
	}
	if m.status != "" {
		body = lipgloss.JoinVertical(lipgloss.Left, body, m.status)
	}
	return body
}

// discoverReposCmd scans the default location for repositories. Discovery is
// fast and cancellation tears down the whole program, so it builds its own ctx.
func discoverReposCmd() tea.Cmd {
	return func() tea.Msg {
		st, err := state.Load()
		if err != nil {
			return reposLoadedMsg{err: fmt.Errorf("load state: %w", err)}
		}
		result, err := discover.Repos(context.Background(), st)
		return reposLoadedMsg{result: result, err: err}
	}
}

func applyReposCmd(r host.Runner, sel apply.RepoSelection) tea.Cmd {
	return func() tea.Msg {
		results, err := apply.ApplyRepos(context.Background(), r, sel)
		return reposAppliedMsg{results: results, err: err}
	}
}

func scanDir() string {
	st, err := state.Load()
	if err != nil {
		return "?"
	}
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return "?"
	}
	return dl
}

func applySummary(results []reconcile.Result, err error) string {
	var cloned, present, errs int
	for _, r := range results {
		switch {
		case r.Err != nil:
			errs++
		case r.Action == reconcile.ActionCloned:
			cloned++
		case r.Action == reconcile.ActionPresent:
			present++
		}
	}
	summary := fmt.Sprintf("applied: %d cloned, %d present, %d error(s)", cloned, present, errs)
	if err != nil {
		return statusErr.Render(summary + ": " + err.Error())
	}
	if errs > 0 {
		return statusErr.Render(summary)
	}
	return statusOK.Render(summary)
}
