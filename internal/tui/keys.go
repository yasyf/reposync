package tui

import "github.com/charmbracelet/bubbles/key"

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
