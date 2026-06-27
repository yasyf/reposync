package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"

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
