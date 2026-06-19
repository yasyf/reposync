package tui

import "github.com/charmbracelet/bubbles/key"

// globalKeyMap holds the router-level bindings handled by rootModel when no
// screen is in a modal sub-state.
type globalKeyMap struct {
	NextTab key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func newGlobalKeyMap() globalKeyMap {
	return globalKeyMap{
		NextTab: key.NewBinding(key.WithKeys("tab", "n"), key.WithHelp("tab", "switch tab")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// reposKeyMap holds the repos screen's contextual bindings.
type reposKeyMap struct {
	Filter key.Binding
	Toggle key.Binding
	Apply  key.Binding
	Sort   key.Binding
	Yes    key.Binding
	No     key.Binding
}

func newReposKeyMap() reposKeyMap {
	return reposKeyMap{
		Filter: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Toggle: key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
		Apply:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
		Sort:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sort")),
		Yes:    key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")),
		No:     key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n", "cancel")),
	}
}

// hostsKeyMap holds the hosts screen's contextual bindings.
type hostsKeyMap struct {
	Filter  key.Binding
	Add     key.Binding
	Verify  key.Binding
	Select  key.Binding
	Remove  key.Binding
	Confirm key.Binding
	Cancel  key.Binding
}

func newHostsKeyMap() hostsKeyMap {
	return hostsKeyMap{
		Filter:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Add:     key.NewBinding(key.WithKeys("+"), key.WithHelp("+", "add host")),
		Verify:  key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "verify all")),
		Select:  key.NewBinding(key.WithKeys("enter", " "), key.WithHelp("enter", "verify/edit")),
		Remove:  key.NewBinding(key.WithKeys("r", "delete", "backspace"), key.WithHelp("r", "remove")),
		Confirm: key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")),
		Cancel:  key.NewBinding(key.WithKeys("n", "esc", "ctrl+c"), key.WithHelp("esc", "cancel")),
	}
}
