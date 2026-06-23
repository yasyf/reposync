package hostregistry

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/brutella/dnssd"
)

const bonjourTimeout = 3 * time.Second

// SkipNote records a candidate that a scan skipped, and why. Scans surface
// these instead of aborting so one unreadable entry never hides the rest.
type SkipNote struct {
	Name   string // directory entry, node, or host label that was skipped
	Reason string
}

// HostCandidate is one host discovered on the network.
type HostCandidate struct {
	Node          string // tailscale/bonjour node label
	DefaultTarget string // suggested ssh target, e.g. "user@node"
	Source        string // "tailscale" or "bonjour"
	Online        bool   // reported reachable by the discovery source
	Registered    bool   // already present in the registry (matched by node label)
}

// HostResult is the outcome of discovering candidate hosts on the network.
type HostResult struct {
	Candidates []HostCandidate
	Notes      []SkipNote
}

// Hosts enumerates candidate hosts on the network from tailscale and Bonjour,
// dedupes them, and marks which are already named in registered. Discovery is
// best-effort: a missing or failing source degrades to a SkipNote rather than an
// error, so Hosts never returns a non-nil error (the deliberate exception to
// fail-fast — there is nothing to fix when a source is simply absent).
func Hosts(ctx context.Context, r Runner, registered []string) (HostResult, error) {
	var notes []SkipNote

	out, err := r.Local(ctx, "id", "-un")
	localUser := strings.TrimSpace(out)
	if err != nil {
		notes = append(notes, SkipNote{Name: "id", Reason: err.Error()})
	}

	tsCands, tsNotes := discoverTailscale(ctx, r, localUser)
	notes = append(notes, tsNotes...)

	bjCands, bjNotes := discoverBonjour(ctx, localUser)
	notes = append(notes, bjNotes...)

	cands := make([]HostCandidate, 0, len(tsCands)+len(bjCands))
	cands = append(cands, tsCands...)
	cands = append(cands, bjCands...)

	return HostResult{Candidates: mergeHosts(cands, registered), Notes: notes}, nil
}

// discoverTailscale enumerates online and offline peers from `tailscale status
// --json`. A failure to run or parse tailscale degrades to a single SkipNote.
func discoverTailscale(ctx context.Context, r Runner, localUser string) ([]HostCandidate, []SkipNote) {
	out, err := r.Local(ctx, "tailscale", "status", "--json")
	if err != nil {
		return nil, []SkipNote{{Name: "tailscale", Reason: err.Error()}}
	}
	var status struct {
		Peer map[string]tailscalePeer
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, []SkipNote{{Name: "tailscale", Reason: err.Error()}}
	}

	var cands []HostCandidate
	for _, p := range status.Peer {
		node := TailscaleNode(p.DNSName)
		if node == "" {
			continue
		}
		if isMullvad(p) {
			continue
		}
		if isLikelyEphemeral(p) {
			continue
		}
		cands = append(cands, HostCandidate{
			Node:          node,
			DefaultTarget: target(localUser, node),
			Source:        "tailscale",
			Online:        p.Online,
		})
	}
	return cands, nil
}

// tailscalePeer is the subset of a `tailscale status --json` peer that discovery
// decodes. Location is a pointer so a JSON null (a personal node) is
// distinguishable from a populated object (a Mullvad relay); KeyExpiry is a
// pointer so a null/absent expiry is distinguishable from a real one.
type tailscalePeer struct {
	DNSName   string
	HostName  string
	Online    bool
	OS        string
	Tags      []string
	KeyExpiry *time.Time
	Location  *struct{}
}

// isMullvad reports whether p is a Mullvad relay rather than a personal node.
// Mullvad peers carry a populated Location and the mullvad-exit-node tag, while
// personal nodes report Location=null and no such tag; either signal alone marks
// a relay, so they are OR'd.
func isMullvad(p tailscalePeer) bool {
	if p.Location != nil {
		return true
	}
	for _, t := range p.Tags {
		if t == "tag:mullvad-exit-node" {
			return true
		}
	}
	return false
}

// isLikelyEphemeral best-effort flags an ephemeral peer. `tailscale status
// --json` carries no Ephemeral field, so a nil KeyExpiry is the only hint, and it
// is not a clean signal (a personal node with key expiry disabled reports it too);
// the guard therefore fires only on non-Mullvad peers.
func isLikelyEphemeral(p tailscalePeer) bool {
	return !isMullvad(p) && p.KeyExpiry == nil
}

// discoverBonjour browses _ssh._tcp services for up to bonjourTimeout. The lookup
// blocks until the context expires, so a deadline or cancellation is normal
// completion; any other error degrades to a SkipNote.
func discoverBonjour(ctx context.Context, localUser string) ([]HostCandidate, []SkipNote) {
	ctx2, cancel := context.WithTimeout(ctx, bonjourTimeout)
	defer cancel()

	seen := map[string]struct{}{}
	var cands []HostCandidate
	add := func(e dnssd.BrowseEntry) {
		node := bonjourNode(e)
		if node == "" {
			return
		}
		if _, dup := seen[node]; dup {
			return
		}
		seen[node] = struct{}{}
		cands = append(cands, HostCandidate{
			Node:          node,
			DefaultTarget: target(localUser, node),
			Source:        "bonjour",
			Online:        true,
		})
	}
	rmv := func(dnssd.BrowseEntry) {}

	err := dnssd.LookupType(ctx2, "_ssh._tcp.local.", add, rmv)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return cands, []SkipNote{{Name: "bonjour", Reason: err.Error()}}
	}
	return cands, nil
}

// mergeHosts dedupes candidates by node — preferring a tailscale entry over a
// bonjour one for the same node — marks each as Registered when registered
// already names that node, and returns them sorted by node.
func mergeHosts(cands []HostCandidate, registered []string) []HostCandidate {
	regNodes := map[string]struct{}{}
	for _, h := range registered {
		regNodes[hostNode(h)] = struct{}{}
	}

	byNode := map[string]HostCandidate{}
	for _, c := range cands {
		existing, ok := byNode[c.Node]
		if ok && (c.Source != "tailscale" || existing.Source != "bonjour") {
			continue
		}
		byNode[c.Node] = c
	}

	merged := make([]HostCandidate, 0, len(byNode))
	for _, c := range byNode {
		_, c.Registered = regNodes[c.Node]
		merged = append(merged, c)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Node < merged[j].Node
	})
	return merged
}

// bonjourNode derives a node label from a browse entry's host, falling back to
// its instance name, stripping a trailing ".local." and any remaining domain.
func bonjourNode(e dnssd.BrowseEntry) string {
	raw := e.Host
	if raw == "" {
		raw = e.Name
	}
	raw = strings.TrimSuffix(raw, ".")
	raw = strings.TrimSuffix(raw, ".local")
	label, _, _ := strings.Cut(raw, ".")
	return label
}

// hostNode extracts the node label from a registered host target, which is either
// "user@node" or a bare "node": the substring after the last '@'.
func hostNode(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		return target[i+1:]
	}
	return target
}

// target builds the suggested ssh target, degrading to the bare node when the
// local user is unknown.
func target(localUser, node string) string {
	if localUser == "" {
		return node
	}
	return localUser + "@" + node
}
