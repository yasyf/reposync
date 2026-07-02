package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	synckittui "github.com/yasyf/synckit/tui"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
)

// defaultIdle is the fallback "recent activity" window when none is configured.
const defaultIdle = 10 * time.Minute

// repoStatus is the live VCS state of a repo row, learned asynchronously.
type repoStatus int

const (
	statusUnknown repoStatus = iota // probe not yet returned
	statusClean                     // idle and disposable
	statusActive                    // recent activity within the idle window
	statusDirty                     // in-progress work in the tree
	statusError                     // the probe itself failed
)

// glyph renders the status as a short colored marker for a list row.
func (s repoStatus) glyph() string {
	switch s {
	case statusClean:
		return synckittui.BadgeClean.Render("✓")
	case statusActive:
		return synckittui.BadgeSync.Render("⟳")
	case statusDirty:
		return synckittui.BadgeDirty.Render("●")
	case statusError:
		return synckittui.GlyphFail.Render("✗")
	default:
		return synckittui.Dim.Render("·")
	}
}

// label renders the status as a word for the detail pane.
func (s repoStatus) label() string {
	switch s {
	case statusClean:
		return synckittui.BadgeClean.Render("clean")
	case statusActive:
		return synckittui.BadgeSync.Render("active")
	case statusDirty:
		return synckittui.BadgeDirty.Render("dirty")
	case statusError:
		return synckittui.GlyphFail.Render("error")
	default:
		return synckittui.Dim.Render("checking…")
	}
}

// sortMode orders the repo list. recent (default) floats the most recently
// edited repos to the top; name is alphabetical; status surfaces dirty repos.
type sortMode int

const (
	sortRecent sortMode = iota
	sortName
	sortStatus
)

func (s sortMode) String() string {
	switch s {
	case sortName:
		return "name"
	case sortStatus:
		return "status"
	default:
		return "recently edited"
	}
}

func (s sortMode) next() sortMode { return (s + 1) % 3 }

// repoMTime returns the newest modification time among a repo's VCS metadata
// leaf directories — the same set the watch daemon watches — falling back to the
// checkout root. Any stat error is skipped so a fresh clone, a packed-refs repo,
// or a synthetic test candidate never breaks the scan.
func repoMTime(absPath string) time.Time {
	leaves := append(vcs.WatchPaths(absPath), absPath)

	var newest time.Time
	for _, p := range leaves {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// repoStatusCmd probes one repo's live VCS state and last-activity time off the
// UI thread. The whole program tears down on quit, so it builds its own ctx; the
// generation stamp lets the model drop results from a superseded scan.
func repoStatusCmd(absPath, relpath, trunk string, idle time.Duration, gen int) tea.Cmd {
	return func() tea.Msg {
		r, err := vcs.Open(absPath, trunk)
		if err != nil {
			return repoStatusMsg{relpath: relpath, generation: gen, err: err}
		}
		ctx := context.Background()
		busy, reason, err := r.InUse(ctx, idle)
		if err != nil {
			return repoStatusMsg{relpath: relpath, generation: gen, err: err}
		}
		activity, _ := r.LastActivity(ctx)
		return repoStatusMsg{
			relpath:    relpath,
			generation: gen,
			status:     classifyStatus(busy, reason),
			reason:     reason,
			activity:   activity,
		}
	}
}

// classifyStatus maps an InUse verdict onto a row status: a dirty tree dominates,
// other in-use repos are merely active, and an idle repo is clean.
func classifyStatus(busy bool, reason string) repoStatus {
	if !busy {
		return statusClean
	}
	if strings.Contains(reason, "dirty") {
		return statusDirty
	}
	return statusActive
}

// loadIdleThreshold reads the configured idle window used to classify "recent
// activity"; a missing config or unset value falls back to a sane default.
func loadIdleThreshold() time.Duration {
	st, err := state.Load()
	if err != nil {
		return defaultIdle
	}
	if d := time.Duration(st.Settings.IdleThreshold); d > 0 {
		return d
	}
	return defaultIdle
}

// sortRepoItems orders a list of repoItems in place by the active sort mode.
func sortRepoItems(items []list.Item, mode sortMode) {
	sort.SliceStable(items, func(a, b int) bool {
		ia, ib := items[a].(repoItem), items[b].(repoItem)
		switch mode {
		case sortName:
			return ia.cand.Relpath < ib.cand.Relpath
		case sortStatus:
			if ia.status != ib.status {
				return ia.status > ib.status
			}
			return ia.sortKey().After(ib.sortKey())
		default:
			ka, kb := ia.sortKey(), ib.sortKey()
			if !ka.Equal(kb) {
				return ka.After(kb)
			}
			return ia.cand.Relpath < ib.cand.Relpath
		}
	})
}

// selectedRelpath reports the relpath of the cursor row, or "" when the list is
// empty, so a re-sort can restore the selection.
func selectedRelpath(l list.Model) string {
	if it, ok := l.SelectedItem().(repoItem); ok {
		return it.cand.Relpath
	}
	return ""
}

// selectRelpath moves the cursor back onto the row with the given relpath.
func selectRelpath(l *list.Model, relpath string) {
	if relpath == "" {
		return
	}
	for i, raw := range l.Items() {
		if it, ok := raw.(repoItem); ok && it.cand.Relpath == relpath {
			l.Select(i)
			return
		}
	}
}

// relTime renders a timestamp as a short "2m ago" relative string; the zero time
// reads as unknown.
func relTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
