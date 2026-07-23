// Package discover scans the default location for git/jj repositories. It is
// read-only.
package discover

// SkipNote records a candidate that a scan skipped, and why. Scans surface
// these instead of aborting so one unreadable entry never hides the rest.
type SkipNote struct {
	Name   string
	Reason string
}

// Candidate is one repository found under the default location.
type Candidate struct {
	Relpath   string // path relative to default_location (the state key)
	AbsPath   string // absolute checkout path
	Kind      string // "jj" or "git"
	Origin    string // origin remote URL, or "" when local-only
	LocalOnly bool   // true when the repo has no origin remote
	Tracked   bool   // already present in state.Repos
	NoEnvSync bool   // tracked repo opted out of env-file sync
}

// RepoResult is the outcome of scanning the default location for repositories.
type RepoResult struct {
	Candidates []Candidate
	Skipped    []SkipNote
}
