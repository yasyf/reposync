package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/reconcile"
)

func TestStartAddFocusesInput(t *testing.T) {
	m := newHostsModel(Options{})
	s, _ := m.startAdd("")
	hm := s.(hostsModel)
	if !hm.input.Focused() {
		t.Fatal("startAdd must focus the input so the host target can be typed")
	}
	// A keystroke must reach the (focused) input, not be swallowed.
	s, _ = hm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if got := s.(hostsModel).input.Value(); got != "x" {
		t.Fatalf("after typing into the add-host input, value = %q, want %q", got, "x")
	}
}

func TestValidateTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "user@node", in: "yasyf@yasyf-home", ok: true},
		{name: "bare node", in: "yasyf-home", ok: true},
		{name: "dotted node", in: "node.tailnet.ts.net", ok: true},
		{name: "underscore user", in: "ad_min@node", ok: true},
		{name: "empty", in: "", ok: false},
		{name: "whitespace only", in: "   ", ok: false},
		{name: "embedded space", in: "user @node", ok: false},
		{name: "trailing space", in: "node ", ok: false},
		{name: "leading at", in: "@node", ok: false},
		{name: "node starts with hyphen", in: "-node", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTarget(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("validateTarget(%q) = %v, want nil", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validateTarget(%q) = nil, want error", tc.in)
			}
		})
	}
}

func TestHostNode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"yasyf@alpha", "alpha"},
		{"alpha", "alpha"},
		{"user@host@weird", "weird"},
	}
	for _, tc := range cases {
		if got := hostNode(tc.in); got != tc.want {
			t.Fatalf("hostNode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClassifyVerify(t *testing.T) {
	cases := []struct {
		name string
		res  host.VerifyResult
		want verifyState
	}{
		{name: "ready", res: host.VerifyResult{Reachable: true, Bootstrapped: true}, want: verifyOK},
		{name: "reachable not installed", res: host.VerifyResult{Reachable: true}, want: verifyWarn},
		{name: "unreachable", res: host.VerifyResult{Err: errors.New("connection refused")}, want: verifyFail},
		{name: "unreachable but bootstrapped flag ignored", res: host.VerifyResult{Bootstrapped: true}, want: verifyFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyVerify(tc.res); got != tc.want {
				t.Fatalf("classifyVerify(%+v) = %v, want %v", tc.res, got, tc.want)
			}
		})
	}
}

func TestMergeHostItems(t *testing.T) {
	cands := []discover.HostCandidate{
		{Node: "alpha", DefaultTarget: "yasyf@alpha", Source: "tailscale", Online: true, Registered: true},
		{Node: "beta", DefaultTarget: "yasyf@beta", Source: "bonjour", Online: false, Registered: false},
	}
	// gamma is registered but undiscovered; delta is already covered by the
	// "beta" candidate's node so registration must not duplicate it.
	registered := []string{"yasyf@gamma", "yasyf@beta"}

	items := mergeHostItems(cands, registered)

	if len(items) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(items), items)
	}

	alpha := items[0]
	if alpha.node != "alpha" || alpha.target != "yasyf@alpha" || alpha.source != "tailscale" {
		t.Fatalf("alpha = %+v, want node=alpha target=yasyf@alpha source=tailscale", alpha)
	}
	if !alpha.online || !alpha.registered {
		t.Fatalf("alpha online/registered = %v/%v, want true/true", alpha.online, alpha.registered)
	}

	beta := items[1]
	if beta.registered {
		t.Fatalf("beta registered = true, want false (candidate carried Registered=false)")
	}

	gamma := items[2]
	if gamma.node != "gamma" || gamma.target != "yasyf@gamma" {
		t.Fatalf("gamma = %+v, want node=gamma target=yasyf@gamma", gamma)
	}
	if gamma.source != "registered" || gamma.online || !gamma.registered {
		t.Fatalf("gamma = %+v, want source=registered online=false registered=true", gamma)
	}
}

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
	m := newReposModel(Options{})
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
