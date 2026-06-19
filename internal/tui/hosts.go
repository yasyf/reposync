package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/service"
	"github.com/yasyf/reposync/internal/state"
)

const (
	hostModeList = iota
	hostModeAdd
	hostModeBootstrapping
)

const verifyLegend = "✓ ready  ⚠ reachable, not installed  ✗ unreachable  … checking"

type hostsModel struct {
	opts       Options
	list       list.Model
	allItems   []list.Item
	filter     filterBar
	loading    bool
	mode       int
	input      textinput.Model
	spin       spinner.Model
	logVP      viewport.Model
	logLines   []string
	busyTarget string
	cancel     context.CancelFunc
	lines      chan string
	confirm    *hostConfirmState
	status     string
	width      int
	height     int
	keys       hostsKeyMap

	mdListW      int
	mdDetailW    int
	mdHeight     int
	mdShowDetail bool
}

// hostsReserve is the rows the hosts screen keeps below the master-detail split
// for the verify legend and status line.
const hostsReserve = 2

// hostConfirmState is an open removal confirmation awaiting its target.
type hostConfirmState struct {
	prompt string
	target string
}

func newHostsModel(opts Options) hostsModel {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	in := textinput.New()
	in.Placeholder = "user@node"
	in.Validate = validateTarget
	l := list.New(nil, hostDelegate{}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	return hostsModel{opts: opts, list: l, filter: newFilterBar(), loading: true, input: in, spin: sp, keys: newHostsKeyMap()}
}

func (m hostsModel) Title() string { return "Hosts" }

func (m hostsModel) Help() []key.Binding {
	switch m.mode {
	case hostModeAdd:
		return []key.Binding{m.keys.Cancel}
	case hostModeBootstrapping:
		return []key.Binding{m.keys.Cancel}
	}
	if m.confirm != nil {
		return []key.Binding{m.keys.Confirm, m.keys.Cancel}
	}
	return []key.Binding{m.keys.Filter, m.keys.Add, m.keys.Select, m.keys.Verify, m.keys.Remove}
}

func (m hostsModel) wantsKey(tea.KeyMsg) bool {
	return m.mode == hostModeAdd || m.mode == hostModeBootstrapping || m.confirm != nil || m.filter.Focused()
}

func (m hostsModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, discoverHostsCmd(m.opts.Runner))
}

func (m hostsModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.mdListW, m.mdDetailW, m.mdHeight, m.mdShowDetail = splitDims(msg.Width, msg.Height-filterBarLines-hostsReserve)
		m.list.SetSize(m.mdListW, m.mdHeight)
		m.logVP = viewport.New(msg.Width, max(1, msg.Height-2))
		return m, nil

	case hostsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = statusErr.Render(msg.err.Error())
			return m, nil
		}
		m.allItems = toListItems(msg.items)
		cmd := m.refreshHosts()
		return m, tea.Batch(cmd, verifyAllCmd(m.opts.Runner, msg.items))

	case hostVerifiedMsg:
		cmd := m.markVerified(msg.target, msg.res)
		return m, cmd

	case hostAddProgressMsg:
		if m.lines == nil {
			return m, nil
		}
		m.logLines = append(m.logLines, msg.line)
		m.logVP.SetContent(strings.Join(m.logLines, "\n"))
		m.logVP.GotoBottom()
		return m, waitForLine(m.lines)

	case hostAddDoneMsg:
		m.mode = hostModeList
		m.cancel = nil
		m.lines = nil
		if msg.err != nil {
			m.status = statusErr.Render("bootstrap failed: " + msg.err.Error())
		} else {
			m.status = statusOK.Render("bootstrapped " + msg.target)
		}
		return m, discoverHostsCmd(m.opts.Runner)

	case hostRemovedMsg:
		if msg.err != nil {
			m.status = statusErr.Render("remove failed: " + msg.err.Error())
			return m, nil
		}
		m.status = statusOK.Render("removed " + msg.target)
		return m, discoverHostsCmd(m.opts.Runner)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.loading || m.mode == hostModeBootstrapping {
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

func (m hostsModel) handleKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	switch m.mode {
	case hostModeAdd:
		return m.handleAddKey(msg)
	case hostModeBootstrapping:
		if key.Matches(msg, m.keys.Cancel) && m.cancel != nil {
			m.cancel()
		}
		return m, nil
	}

	if m.confirm != nil {
		switch {
		case key.Matches(msg, m.keys.Confirm):
			target := m.confirm.target
			m.confirm = nil
			return m, removeHostCmd(target)
		case key.Matches(msg, m.keys.Cancel):
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
	case key.Matches(msg, m.keys.Add):
		return m.startAdd("")
	case key.Matches(msg, m.keys.Verify):
		return m, verifyAllCmd(m.opts.Runner, listItems(m.list))
	case key.Matches(msg, m.keys.Remove):
		return m.startRemove()
	case key.Matches(msg, m.keys.Select):
		return m.selectRow()
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// handleFilterKey routes keys while the filter bar holds focus: esc clears and
// blurs, enter blurs keeping the filter, anything else edits the query and
// re-narrows the list live.
func (m hostsModel) handleFilterKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filter.Blur()
		m.filter.Clear()
		cmd := m.refreshHosts()
		return m, cmd
	case tea.KeyEnter:
		m.filter.Blur()
		return m, nil
	}
	var icmd tea.Cmd
	m.filter, icmd = m.filter.Update(msg)
	rcmd := m.refreshHosts()
	return m, tea.Batch(icmd, rcmd)
}

// refreshHosts recomputes the visible list from the canonical slice under the
// active filter, keeping the cursor on the same host.
func (m *hostsModel) refreshHosts() tea.Cmd {
	sel := selectedTarget(m.list)
	visible := filterItems(m.allItems, m.filter.Value())
	cmd := m.list.SetItems(visible)
	selectTarget(&m.list, sel)
	return cmd
}

// selectedTarget reports the target of the cursor row, or "" when the list is
// empty, so a re-render can restore the selection.
func selectedTarget(l list.Model) string {
	if it, ok := l.SelectedItem().(hostItem); ok {
		return it.target
	}
	return ""
}

// selectTarget moves the cursor back onto the row with the given target.
func selectTarget(l *list.Model, target string) {
	if target == "" {
		return
	}
	for i, raw := range l.Items() {
		if it, ok := raw.(hostItem); ok && it.target == target {
			l.Select(i)
			return
		}
	}
}

func (m hostsModel) handleAddKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Cancel):
		m.mode = hostModeList
		m.input.Blur()
		return m, nil
	case msg.Type == tea.KeyEnter:
		target := strings.TrimSpace(m.input.Value())
		if err := validateTarget(target); err != nil {
			m.status = statusErr.Render(err.Error())
			return m, nil
		}
		m.input.Blur()
		return m.startBootstrap(target)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m hostsModel) selectRow() (screen, tea.Cmd) {
	it, ok := m.list.SelectedItem().(hostItem)
	if !ok {
		return m, nil
	}
	if it.registered {
		ccmd := m.markChecking(it.target)
		return m, tea.Batch(ccmd, verifyHostCmd(m.opts.Runner, it.target))
	}
	return m.startAdd(it.target)
}

func (m hostsModel) startAdd(prefill string) (screen, tea.Cmd) {
	m.mode = hostModeAdd
	m.input.SetValue(prefill)
	m.input.CursorEnd()
	cmd := m.input.Focus()
	return m, cmd
}

func (m hostsModel) startRemove() (screen, tea.Cmd) {
	it, ok := m.list.SelectedItem().(hostItem)
	if !ok || !it.registered {
		return m, nil
	}
	m.confirm = &hostConfirmState{
		prompt: fmt.Sprintf("Remove host %s? (y/N)", it.target),
		target: it.target,
	}
	return m, nil
}

func (m hostsModel) startBootstrap(target string) (screen, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.mode = hostModeBootstrapping
	m.busyTarget = target
	m.cancel = cancel
	m.lines = make(chan string, 64)
	m.logLines = nil
	m.logVP.SetContent("")
	return m, tea.Batch(m.spin.Tick, addHostCmd(ctx, m.opts.Runner, target, m.lines), waitForLine(m.lines))
}

func (m *hostsModel) markChecking(target string) tea.Cmd {
	for i, raw := range m.allItems {
		it := raw.(hostItem)
		if it.target == target {
			it.state = verifyChecking
			m.allItems[i] = it
			break
		}
	}
	return m.refreshHosts()
}

func (m *hostsModel) markVerified(target string, res host.VerifyResult) tea.Cmd {
	for i, raw := range m.allItems {
		it := raw.(hostItem)
		if it.target == target {
			it.verify = res
			it.state = classifyVerify(res)
			m.allItems[i] = it
			break
		}
	}
	return m.refreshHosts()
}

func (m hostsModel) View() string {
	switch m.mode {
	case hostModeAdd:
		hint := dim.Render("enter to bootstrap · esc to cancel")
		return lipgloss.JoinVertical(lipgloss.Left, "Add host:", m.input.View(), hint, m.status)
	case hostModeBootstrapping:
		head := m.spin.View() + " Bootstrapping " + m.busyTarget + dim.Render(" (esc to cancel)")
		return lipgloss.JoinVertical(lipgloss.Left, head, logPane.Render(m.logVP.View()))
	}

	if m.loading {
		return m.spin.View() + " Discovering hosts…"
	}
	if len(m.list.Items()) == 0 {
		return dim.Render("No hosts discovered. Press + to add one.")
	}

	split := masterDetail(m.list.View(), renderHostDetail(m.list.SelectedItem()), m.mdListW, m.mdDetailW, m.mdHeight, m.mdShowDetail)
	body := lipgloss.JoinVertical(lipgloss.Left, m.filter.View(len(m.list.Items()), len(m.allItems)), split, dim.Render(verifyLegend))
	if m.confirm != nil {
		body = lipgloss.JoinVertical(lipgloss.Left, body, confirmBox.Render(m.confirm.prompt))
	}
	if m.status != "" {
		body = lipgloss.JoinVertical(lipgloss.Left, body, m.status)
	}
	return body
}

func toListItems(items []hostItem) []list.Item {
	out := make([]list.Item, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out
}

func listItems(l list.Model) []hostItem {
	raw := l.Items()
	out := make([]hostItem, len(raw))
	for i, r := range raw {
		out[i] = r.(hostItem)
	}
	return out
}

// discoverHostsCmd scans the network for hosts and merges in any registered
// host that discovery did not surface.
func discoverHostsCmd(r host.Runner) tea.Cmd {
	return func() tea.Msg {
		st, err := state.Load()
		if err != nil {
			return hostsLoadedMsg{err: fmt.Errorf("load state: %w", err)}
		}
		result, err := discover.Hosts(context.Background(), r, st)
		if err != nil {
			return hostsLoadedMsg{err: err}
		}
		return hostsLoadedMsg{items: mergeHostItems(result.Candidates, st.Hosts)}
	}
}

// mergeHostItems turns discovery candidates into rows and appends every
// registered host that discovery missed as an offline registered row.
func mergeHostItems(cands []discover.HostCandidate, registered []string) []hostItem {
	items := make([]hostItem, 0, len(cands)+len(registered))
	seen := map[string]struct{}{}
	for _, c := range cands {
		items = append(items, hostItem{
			node:       c.Node,
			target:     c.DefaultTarget,
			source:     c.Source,
			online:     c.Online,
			registered: c.Registered,
		})
		seen[c.Node] = struct{}{}
	}
	for _, h := range registered {
		if _, ok := seen[hostNode(h)]; ok {
			continue
		}
		items = append(items, hostItem{
			node:       hostNode(h),
			target:     h,
			source:     "registered",
			online:     false,
			registered: true,
		})
	}
	return items
}

func verifyAllCmd(r host.Runner, items []hostItem) tea.Cmd {
	var cmds []tea.Cmd
	for _, it := range items {
		if it.registered {
			cmds = append(cmds, verifyHostCmd(r, it.target))
		}
	}
	return tea.Batch(cmds...)
}

func verifyHostCmd(r host.Runner, target string) tea.Cmd {
	return func() tea.Msg {
		return hostVerifiedMsg{target: target, res: host.Verify(context.Background(), r, target)}
	}
}

func removeHostCmd(target string) tea.Cmd {
	return func() tea.Msg {
		return hostRemovedMsg{target: target, err: host.RemoveHost(target)}
	}
}

// addHostCmd runs the bootstrap, streaming each step onto lines and closing the
// channel when the run ends so waitForLine unblocks.
func addHostCmd(ctx context.Context, r host.Runner, target string, lines chan string) tea.Cmd {
	return func() tea.Msg {
		st, err := state.Load()
		if err != nil {
			close(lines)
			return hostAddDoneMsg{target: target, err: fmt.Errorf("load state: %w", err)}
		}
		log, err := host.AddHostStream(ctx, st, r, target, "", false, func(s string) {
			lines <- s
		})
		// Bring this host online too: adding a peer should leave the local
		// reconcile tick and watch daemon running without a separate install.
		if err == nil {
			if ierr := service.Install(ctx, service.NewLauncher(), false); ierr != nil {
				lines <- fmt.Sprintf("WARN install local services: %v", ierr)
			} else {
				lines <- "installed local services"
			}
		}
		close(lines)
		return hostAddDoneMsg{target: target, log: log, err: err}
	}
}

// waitForLine blocks on the next bootstrap step; a closed channel yields no
// message, leaving hostAddDoneMsg to end the run.
func waitForLine(lines chan string) tea.Cmd {
	return func() tea.Msg {
		if lines == nil {
			return nil
		}
		line, ok := <-lines
		if !ok {
			return nil
		}
		return hostAddProgressMsg{line: line}
	}
}

// hostNode extracts the node label from a "user@node" or bare "node" target.
func hostNode(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		return target[i+1:]
	}
	return target
}
