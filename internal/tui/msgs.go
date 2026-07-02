package tui

import (
	"time"

	"github.com/yasyf/reposync/internal/discover"
	"github.com/yasyf/reposync/internal/reconcile"
)

// reposLoadedMsg carries the outcome of a repo discovery scan and the
// configured idle threshold from the same state load.
type reposLoadedMsg struct {
	result discover.RepoResult
	idle   time.Duration
	err    error
}

// repoStatusMsg carries one repo's live VCS state and last-activity time from an
// async probe. generation stamps the scan it belongs to so a superseded scan's
// late results are dropped.
type repoStatusMsg struct {
	relpath    string
	status     repoStatus
	reason     string
	activity   time.Time
	err        error
	generation int
}

// reposAppliedMsg carries the outcome of applying a repo selection.
type reposAppliedMsg struct {
	results []reconcile.Result
	err     error
}
