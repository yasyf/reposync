// Package discover scans the default location for git/jj repositories and the
// network for candidate hosts (Tailscale peers, Bonjour _ssh._tcp services),
// reporting which are already tracked in state. It is read-only.
package discover

// SkipNote records a candidate that a scan skipped, and why. Scans surface
// these instead of aborting so one unreadable entry never hides the rest.
type SkipNote struct {
	Name   string // directory entry, node, or host label that was skipped
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
}

// RepoResult is the outcome of scanning the default location for repositories.
type RepoResult struct {
	Candidates []Candidate
	Skipped    []SkipNote
}

// HostCandidate is one host discovered on the network.
type HostCandidate struct {
	Node          string // tailscale/bonjour node label
	DefaultTarget string // suggested ssh target, e.g. "user@node"
	Source        string // "tailscale" or "bonjour"
	Online        bool   // reported reachable by the discovery source
	Registered    bool   // already present in state.Hosts (matched by node label)
}

// HostResult is the outcome of discovering candidate hosts on the network.
type HostResult struct {
	Candidates []HostCandidate
	Notes      []SkipNote
}
