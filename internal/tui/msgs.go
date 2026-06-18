package tui

import (
	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/host"
	"github.com/yasyf/reposync/internal/reconcile"
)

// reposLoadedMsg carries the outcome of a repo discovery scan.
type reposLoadedMsg struct {
	result discover.RepoResult
	err    error
}

// reposAppliedMsg carries the outcome of applying a repo selection.
type reposAppliedMsg struct {
	results []reconcile.Result
	err     error
}

// hostsLoadedMsg carries the merged host rows from a discovery scan.
type hostsLoadedMsg struct {
	items []hostItem
	err   error
}

// hostVerifiedMsg carries one host's verify probe result.
type hostVerifiedMsg struct {
	target string
	res    host.VerifyResult
}

// hostAddProgressMsg carries one bootstrap step line as it happens.
type hostAddProgressMsg struct {
	line string
}

// hostAddDoneMsg carries the final bootstrap log and error for a target.
type hostAddDoneMsg struct {
	target string
	log    []string
	err    error
}

// hostRemovedMsg carries the outcome of unregistering a host.
type hostRemovedMsg struct {
	target string
	err    error
}
