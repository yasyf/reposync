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
	allItems []list.Item
	filter   filterBar
	loading  bool
	applying bool
	spin     spinner.Model
	confirm  *confirmState
	status   string
	empty    bool
	scanDir  string
	keys     reposKeyMap

	sortMode   sortMode
	generation int

	mdListW      int
	mdDetailW    int
	mdHeight     int
	mdShowDetail bool
}

// reposReserve is the rows the repos screen keeps below the master-detail split
// for its status line.
const reposReserve = 1

func newReposModel(opts Options) reposModel {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	l := list.New(nil, repoDelegate{}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	return reposModel{opts: opts, list: l, filter: newFilterBar(), loading: true, spin: sp, keys: newReposKeyMap()}
}

func (m reposModel) Title() string { return "Repos" }

func (m reposModel) Help() []key.Binding {
	if m.confirm != nil {
		return []key.Binding{m.keys.Yes, m.keys.No}
	}
	return []key.Binding{m.keys.Filter, m.keys.Toggle, m.keys.Apply, m.keys.Sort}
}

func (m reposModel) wantsKey(tea.KeyMsg) bool {
	return m.confirm != nil || m.filter.Focused()
}

func (m reposModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, discoverReposCmd())
}

func (m reposModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.mdListW, m.mdDetailW, m.mdHeight, m.mdShowDetail = splitDims(msg.Width, msg.Height-filterBarLines-reposReserve)
		m.list.SetSize(m.mdListW, m.mdHeight)
		return m, nil

	case reposLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = statusErr.Render(msg.err.Error())
			return m, nil
		}
		m.empty = len(msg.result.Candidates) == 0
		m.scanDir = scanDir()
		return m.loadItems(msg.result.Candidates)

	case repoStatusMsg:
		return m.applyStatus(msg)

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

	if m.filter.Focused() {
		return m.handleFilterKey(msg)
	}

	switch {
	case key.Matches(msg, m.keys.Filter):
		cmd := m.filter.Focus()
		return m, cmd
	case key.Matches(msg, m.keys.Toggle):
		return m.toggle()
	case key.Matches(msg, m.keys.Apply):
		return m.apply()
	case key.Matches(msg, m.keys.Sort):
		return m.cycleSort()
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// handleFilterKey routes keys while the filter bar holds focus: esc clears and
// blurs, enter blurs keeping the filter, anything else edits the query and
// re-narrows the list live.
func (m reposModel) handleFilterKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filter.Blur()
		m.filter.Clear()
		cmd := m.refresh()
		return m, cmd
	case tea.KeyEnter:
		m.filter.Blur()
		return m, nil
	}
	var icmd tea.Cmd
	m.filter, icmd = m.filter.Update(msg)
	rcmd := m.refresh()
	return m, tea.Batch(icmd, rcmd)
}

// refresh recomputes the visible list from the canonical slice under the active
// filter and sort, keeping the cursor on the same repo.
func (m *reposModel) refresh() tea.Cmd {
	sel := selectedRelpath(m.list)
	visible := filterItems(m.allItems, m.filter.Value())
	sortRepoItems(visible, m.sortMode)
	cmd := m.list.SetItems(visible)
	selectRelpath(&m.list, sel)
	return cmd
}

// setRepoItems installs a canonical item set and renders it under the active
// filter and sort. It is the seam tests use to stage rows without discovery.
func (m *reposModel) setRepoItems(items []list.Item) {
	m.allItems = items
	m.refresh()
}

// loadItems sorts freshly discovered candidates by the active mode, shows them
// immediately, and fans out an async status probe per repo stamped with the
// current generation so a superseded scan's late results are ignored.
func (m reposModel) loadItems(cands []discover.Candidate) (screen, tea.Cmd) {
	m.generation++
	gen := m.generation

	m.allItems = newRepoItems(cands)

	idle := loadIdleThreshold()
	cmds := []tea.Cmd{m.refresh()}
	for _, raw := range m.allItems {
		it := raw.(repoItem)
		cmds = append(cmds, repoStatusCmd(it.cand.AbsPath, it.cand.Relpath, "", idle, gen))
	}
	return m, tea.Batch(cmds...)
}

// applyStatus folds one async probe result into its canonical row and re-renders
// under the active filter and sort. Results from a superseded scan are dropped.
func (m reposModel) applyStatus(msg repoStatusMsg) (screen, tea.Cmd) {
	if msg.generation != m.generation {
		return m, nil
	}
	for i, raw := range m.allItems {
		it := raw.(repoItem)
		if it.cand.Relpath != msg.relpath {
			continue
		}
		if msg.err != nil {
			it.status = statusError
			it.reason = msg.err.Error()
		} else {
			it.status = msg.status
			it.reason = msg.reason
			it.activity = msg.activity
		}
		m.allItems[i] = it
		break
	}
	cmd := m.refresh()
	return m, cmd
}

// cycleSort advances to the next sort mode and re-renders, preserving the
// selected repo.
func (m reposModel) cycleSort() (screen, tea.Cmd) {
	m.sortMode = m.sortMode.next()
	cmd := m.refresh()
	return m, cmd
}

func (m reposModel) toggle() (screen, tea.Cmd) {
	it, ok := m.list.SelectedItem().(repoItem)
	if !ok {
		return m, nil
	}
	for i, raw := range m.allItems {
		ci := raw.(repoItem)
		if ci.cand.Relpath != it.cand.Relpath {
			continue
		}
		ci.selected = !ci.selected
		m.allItems[i] = ci
		break
	}
	cmd := m.refresh()
	return m, cmd
}

func (m reposModel) apply() (screen, tea.Cmd) {
	var sel apply.RepoSelection
	for _, raw := range m.allItems {
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

	split := masterDetail(m.list.View(), renderRepoDetail(m.list.SelectedItem()), m.mdListW, m.mdDetailW, m.mdHeight, m.mdShowDetail)
	body := lipgloss.JoinVertical(lipgloss.Left, m.filter.View(len(m.list.Items()), len(m.allItems)), split)
	if m.confirm != nil {
		body = lipgloss.JoinVertical(lipgloss.Left, body, confirmBox.Render(m.confirm.prompt))
	}
	if m.applying {
		body = lipgloss.JoinVertical(lipgloss.Left, body, m.spin.View()+" Applying…")
	}
	context := dim.Render("↕ " + m.sortMode.String())
	if m.status != "" {
		context = m.status
	}
	return lipgloss.JoinVertical(lipgloss.Left, body, context)
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
