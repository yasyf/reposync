package tui

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"

	"github.com/yasyf/reposync/internal/discover"
)

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		name   string
		busy   bool
		reason string
		want   repoStatus
	}{
		{name: "idle is clean", busy: false, reason: "", want: statusClean},
		{name: "git dirty tree", busy: true, reason: "dirty working tree", want: statusDirty},
		{name: "jj dirty copy", busy: true, reason: "dirty working copy", want: statusDirty},
		{name: "recent activity is active", busy: true, reason: "recent activity", want: statusActive},
		{name: "jj recent op is active", busy: true, reason: "recent activity: wip", want: statusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStatus(tc.busy, tc.reason); got != tc.want {
				t.Fatalf("classifyStatus(%v, %q) = %v, want %v", tc.busy, tc.reason, got, tc.want)
			}
		})
	}
}

func TestSortRepoItems(t *testing.T) {
	now := time.Now()
	// alpha: oldest mtime, no activity. beta: recent activity beats its mtime.
	// gamma: middling mtime.
	alpha := repoItem{cand: discover.Candidate{Relpath: "alpha"}, mtime: now.Add(-3 * time.Hour), status: statusClean}
	beta := repoItem{cand: discover.Candidate{Relpath: "beta"}, mtime: now.Add(-1 * time.Hour), activity: now.Add(-30 * time.Minute), status: statusDirty}
	gamma := repoItem{cand: discover.Candidate{Relpath: "gamma"}, mtime: now.Add(-2 * time.Hour), status: statusActive}

	cases := []struct {
		name string
		mode sortMode
		want []string
	}{
		{name: "recent floats newest activity then mtime", mode: sortRecent, want: []string{"beta", "gamma", "alpha"}},
		{name: "name is alphabetical", mode: sortName, want: []string{"alpha", "beta", "gamma"}},
		{name: "status surfaces dirty first", mode: sortStatus, want: []string{"beta", "gamma", "alpha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := []list.Item{alpha, gamma, beta}
			sortRepoItems(items, tc.mode)
			got := make([]string, len(items))
			for i, raw := range items {
				got[i] = raw.(repoItem).cand.Relpath
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("%s order = %v, want %v", tc.mode, got, tc.want)
				}
			}
		})
	}
}

func TestApplyStatusGenerationGuard(t *testing.T) {
	m := newReposModel()
	m.generation = 5
	m.setRepoItems([]list.Item{
		repoItem{cand: discover.Candidate{Relpath: "a"}},
		repoItem{cand: discover.Candidate{Relpath: "b"}},
	})

	// A result from a superseded scan is ignored.
	next, _ := m.applyStatus(repoStatusMsg{relpath: "a", status: statusDirty, generation: 4})
	if got := findStatus(next.(reposModel).list, "a"); got != statusUnknown {
		t.Fatalf("stale-generation status applied: got %v, want statusUnknown", got)
	}

	// A result from the current scan updates the row.
	next, _ = m.applyStatus(repoStatusMsg{relpath: "a", status: statusDirty, reason: "dirty working tree", generation: 5})
	if got := findStatus(next.(reposModel).list, "a"); got != statusDirty {
		t.Fatalf("current-generation status not applied: got %v, want statusDirty", got)
	}
}

func TestApplyStatusError(t *testing.T) {
	m := newReposModel()
	m.generation = 1
	m.setRepoItems([]list.Item{repoItem{cand: discover.Candidate{Relpath: "a"}}})

	// A probe error must surface as a distinct error state, not leave the row
	// stuck on the indistinguishable "checking…" (statusUnknown) glyph.
	next, _ := m.applyStatus(repoStatusMsg{relpath: "a", err: errors.New("open failed"), generation: 1})
	if got := findStatus(next.(reposModel).list, "a"); got != statusError {
		t.Fatalf("probe-error status = %v, want statusError", got)
	}
}

func TestApplyStatusRefineSorts(t *testing.T) {
	m := newReposModel()
	m.generation = 1
	now := time.Now()
	m.setRepoItems([]list.Item{
		repoItem{cand: discover.Candidate{Relpath: "old"}, mtime: now.Add(-2 * time.Hour)},
		repoItem{cand: discover.Candidate{Relpath: "new"}, mtime: now.Add(-1 * time.Hour)},
	})

	// "old" reports brand-new activity, so the refine pass must float it to the top.
	next, _ := m.applyStatus(repoStatusMsg{relpath: "old", status: statusClean, activity: now, generation: 1})
	first := next.(reposModel).list.Items()[0].(repoItem)
	if first.cand.Relpath != "old" {
		t.Fatalf("after activity refine, top row = %q, want old", first.cand.Relpath)
	}
}

func TestRepoMTime(t *testing.T) {
	dir := t.TempDir()
	if repoMTime(dir).IsZero() {
		t.Fatal("repoMTime of an existing dir should fall back to the root mtime, not zero")
	}
	if !repoMTime(filepath.Join(dir, "missing")).IsZero() {
		t.Fatal("repoMTime of a missing path should be zero")
	}
}

func TestRelTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero is unknown", in: time.Time{}, want: "unknown"},
		{name: "seconds", in: now.Add(-10 * time.Second), want: "just now"},
		{name: "minutes", in: now.Add(-5 * time.Minute), want: "5m ago"},
		{name: "hours", in: now.Add(-3 * time.Hour), want: "3h ago"},
		{name: "days", in: now.Add(-50 * time.Hour), want: "2d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relTime(tc.in); got != tc.want {
				t.Fatalf("relTime = %q, want %q", got, tc.want)
			}
		})
	}
}

func findStatus(l list.Model, relpath string) repoStatus {
	for _, raw := range l.Items() {
		if it, ok := raw.(repoItem); ok && it.cand.Relpath == relpath {
			return it.status
		}
	}
	return statusUnknown
}
