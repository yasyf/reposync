package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/list"

	"github.com/yasyf/reposync/internal/discover"
)

func TestFilterItems(t *testing.T) {
	repos := []list.Item{
		repoItem{cand: discover.Candidate{Relpath: "aneta/web"}},
		repoItem{cand: discover.Candidate{Relpath: "aneta/repo-sync"}},
		repoItem{cand: discover.Candidate{Relpath: "other/infra"}},
	}
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{name: "empty keeps all", query: "", want: 3},
		{name: "case-insensitive prefix", query: "ANETA", want: 2},
		{name: "narrow to one", query: "web", want: 1},
		{name: "no match", query: "zzz", want: 0},
		{name: "whitespace is trimmed", query: "  repo  ", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(filterItems(repos, tc.query)); got != tc.want {
				t.Fatalf("filterItems(%q) kept %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

func TestFilterItemsHosts(t *testing.T) {
	// The same matcher narrows hosts by target, exercising hostItem.FilterValue.
	hosts := []list.Item{
		hostItem{target: "yasyf@metal"},
		hostItem{target: "yasyf@hermes"},
	}
	if got := len(filterItems(hosts, "met")); got != 1 {
		t.Fatalf("filterItems(hosts, met) kept %d, want 1", got)
	}
}

func TestFilterItemsReturnsFreshSlice(t *testing.T) {
	// An empty query must not alias the input, so sorting the result can't reorder
	// the canonical slice.
	repos := []list.Item{
		repoItem{cand: discover.Candidate{Relpath: "a"}},
		repoItem{cand: discover.Candidate{Relpath: "b"}},
	}
	got := filterItems(repos, "")
	got[0], got[1] = got[1], got[0]
	if repos[0].(repoItem).cand.Relpath != "a" {
		t.Fatal("filterItems aliased its input; reordering the result mutated the source")
	}
}
