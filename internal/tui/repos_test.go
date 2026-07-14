package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/reconcile"
)

func TestApplySummary(t *testing.T) {
	cases := []struct {
		name    string
		results []reconcile.Result
		err     error
		want    string
		isErr   bool
	}{
		{
			name: "all green",
			results: []reconcile.Result{
				{Relpath: "a", Action: reconcile.ActionCloned},
				{Relpath: "b", Action: reconcile.ActionPresent},
				{Relpath: "c", Action: reconcile.ActionCloned},
			},
			want: "applied: 2 cloned, 1 present, 0 error(s)",
		},
		{
			name: "per-repo error counted",
			results: []reconcile.Result{
				{Relpath: "a", Action: reconcile.ActionCloned},
				{Relpath: "b", Action: reconcile.ActionCloned, Err: errors.New("clone failed")},
			},
			want:  "applied: 1 cloned, 0 present, 1 error(s)",
			isErr: true,
		},
		{
			name:    "top-level error",
			results: []reconcile.Result{{Relpath: "a", Action: reconcile.ActionPresent}},
			err:     errors.New("boom"),
			want:    "applied: 0 cloned, 1 present, 0 error(s): boom",
			isErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applySummary(tc.results, tc.err)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("applySummary = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// repoModelWith builds a repos screen whose list holds the given items, bypassing
// discovery so the pending-diff computation can be exercised directly.
func repoModelWith(t *testing.T, items ...repoItem) reposModel {
	t.Helper()
	m := newReposModel()
	raw := make([]list.Item, len(items))
	for i, it := range items {
		raw[i] = it
	}
	m.setRepoItems(raw)
	return m
}

func TestReposApplyPendingDiff(t *testing.T) {
	// selected+untracked -> Enable; tracked+deselected -> Disable; the rest no-op.
	enable := discover.Candidate{Relpath: "new-repo", Origin: "https://x/new.git", Kind: "git"}
	keep := discover.Candidate{Relpath: "kept", Origin: "https://x/kept.git", Kind: "git", Tracked: true}
	drop := discover.Candidate{Relpath: "dropped", Origin: "https://x/dropped.git", Kind: "git", Tracked: true}
	skip := discover.Candidate{Relpath: "untouched", Origin: "https://x/untouched.git", Kind: "git"}

	m := repoModelWith(t,
		repoItem{cand: enable, selected: true},
		repoItem{cand: keep, selected: true},
		repoItem{cand: drop, selected: false},
		repoItem{cand: skip, selected: false},
	)

	next, _ := m.apply()
	rm := next.(reposModel)

	// A disable is pending, so apply() opens the confirm dialog carrying the selection.
	if rm.confirm == nil {
		t.Fatal("apply() with a disable should open a confirm dialog")
	}
	sel := rm.confirm.sel

	if len(sel.Enable) != 1 || sel.Enable[0].Relpath != "new-repo" {
		t.Fatalf("Enable = %+v, want exactly [new-repo]", sel.Enable)
	}
	if len(sel.Disable) != 1 || sel.Disable[0] != "dropped" {
		t.Fatalf("Disable = %+v, want exactly [dropped]", sel.Disable)
	}
}

func TestReposApplyNothingToApply(t *testing.T) {
	// Every row matches its tracked state: no Enable, no Disable, no confirm.
	tracked := discover.Candidate{Relpath: "kept", Origin: "https://x/kept.git", Kind: "git", Tracked: true}
	untracked := discover.Candidate{Relpath: "ignored", Origin: "https://x/ignored.git", Kind: "git"}

	m := repoModelWith(t,
		repoItem{cand: tracked, selected: true},
		repoItem{cand: untracked, selected: false},
	)

	next, cmd := m.apply()
	rm := next.(reposModel)

	if rm.confirm != nil {
		t.Fatalf("apply() with no pending change should not open a confirm dialog")
	}
	if rm.applying {
		t.Fatal("apply() with no pending change should not enter the applying state")
	}
	if cmd != nil {
		t.Fatal("apply() with no pending change should issue no command")
	}
	if !strings.Contains(rm.status, "nothing to apply") {
		t.Fatalf("status = %q, want it to mention nothing to apply", rm.status)
	}
}

// TestRenderRepoDetailShowsEnvSetting proves the detail pane surfaces the env-sync
// opt-out as a settled on/off setting for the selected repo.
func TestRenderRepoDetailShowsEnvSetting(t *testing.T) {
	cases := []struct {
		name      string
		noEnvSync bool
		want      string
	}{
		{"env sync on", false, "on"},
		{"env sync off", true, "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := repoItem{cand: discover.Candidate{Relpath: "alpha", Kind: "git", NoEnvSync: tc.noEnvSync}}
			detail := renderRepoDetail(it)
			if !strings.Contains(detail, "env") {
				t.Fatalf("detail missing env line:\n%s", detail)
			}
			if !strings.Contains(detail, tc.want) {
				t.Fatalf("detail env value = missing %q:\n%s", tc.want, detail)
			}
		})
	}
}

// sizedReposModel builds a loaded repos screen sized to a wxh terminal (the inner
// size the router hands the screen) and stages the given rows. It drives the real
// WindowSizeMsg path so the master-detail split is sized exactly as in production.
func sizedReposModel(t *testing.T, w, h int, items ...repoItem) reposModel {
	t.Helper()
	m := newReposModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = next.(reposModel)
	m.loading = false
	raw := make([]list.Item, len(items))
	for i, it := range items {
		raw[i] = it
	}
	m.setRepoItems(raw)
	return m
}

// TestReposViewHeightFitsBudget pins the whole screen to the terminal's row
// budget: an open confirm box or the applying spinner must be reserved out of the
// split, never pushed past the last row. Relpaths are far wider than the list pane
// so the v0.7.2 no-wrap truncation is exercised too.
func TestReposViewHeightFitsBudget(t *testing.T) {
	const w, h = 80, 30
	long := strings.Repeat("deeply/nested/", 8) + "repo"
	tracked := discover.Candidate{Relpath: long, Origin: "https://x/" + long + ".git", Kind: "git", Tracked: true}
	untracked := discover.Candidate{Relpath: long, Origin: "https://x/" + long + ".git", Kind: "git"}

	t.Run("plain list", func(t *testing.T) {
		m := sizedReposModel(t, w, h, repoItem{cand: tracked, selected: true})
		if got := lipgloss.Height(m.View()); got != h {
			t.Fatalf("plain View height = %d, want %d", got, h)
		}
	})

	t.Run("confirm open", func(t *testing.T) {
		// A tracked repo deselected makes apply() open the disable confirm dialog.
		m := sizedReposModel(t, w, h, repoItem{cand: tracked, selected: false})
		next, _ := m.apply()
		m = next.(reposModel)
		if m.confirm == nil {
			t.Fatal("apply() with a pending disable must open the confirm dialog")
		}
		if got := lipgloss.Height(m.View()); got != h {
			t.Fatalf("confirm-open View height = %d, want %d", got, h)
		}
	})

	t.Run("applying spinner", func(t *testing.T) {
		// An untracked repo selected makes apply() enter the applying state directly.
		m := sizedReposModel(t, w, h, repoItem{cand: untracked, selected: true})
		next, _ := m.apply()
		m = next.(reposModel)
		if !m.applying {
			t.Fatal("an enable-only apply() must enter the applying state")
		}
		if got := lipgloss.Height(m.View()); got != h {
			t.Fatalf("applying View height = %d, want %d", got, h)
		}
	})
}

func TestReposApplyEnableOnlyRunsImmediately(t *testing.T) {
	// An Enable with no Disable skips the confirm dialog and applies right away.
	enable := discover.Candidate{Relpath: "new-repo", Origin: "https://x/new.git", Kind: "git"}

	m := repoModelWith(t, repoItem{cand: enable, selected: true})

	next, cmd := m.apply()
	rm := next.(reposModel)

	if rm.confirm != nil {
		t.Fatal("an enable-only apply must not open a confirm dialog")
	}
	if !rm.applying {
		t.Fatal("an enable-only apply should enter the applying state")
	}
	if cmd == nil {
		t.Fatal("an enable-only apply should issue the apply command")
	}
}

// TestReposKeysIgnoredWhileApplying pins the in-flight apply gate: a second
// enter must not reopen the confirm dialog or race the running apply.
func TestReposKeysIgnoredWhileApplying(t *testing.T) {
	tracked := discover.Candidate{Relpath: "a/repo", Origin: "https://x/a.git", Kind: "git", Tracked: true}

	m := sizedReposModel(t, 80, 30, repoItem{cand: tracked, selected: false})
	next, _ := m.apply()
	m = next.(reposModel)
	if m.confirm == nil {
		t.Fatal("apply() with a pending disable must open the confirm dialog")
	}
	next, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(reposModel)
	if !m.applying || m.confirm != nil {
		t.Fatal("y must start the apply and close the confirm dialog")
	}

	next, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(reposModel)
	if m.confirm != nil {
		t.Fatal("enter during an in-flight apply must not reopen the confirm dialog")
	}
	if !m.applying {
		t.Fatal("the in-flight apply must survive stray keys")
	}
}
