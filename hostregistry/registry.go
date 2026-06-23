package hostregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const maxConcurrentHosts = 8

// Registry is the host-identity slice of state.json: how peers reach this machine
// (Self) and the peers it reaches (Hosts). Load and Update read-modify-write only
// these keys, leaving every other key in the file untouched.
type Registry struct {
	Self  string   `json:"self"`
	Hosts []string `json:"hosts"`
}

// UpsertHost adds a peer ssh target unless it is already registered.
func (g *Registry) UpsertHost(target string) {
	for _, h := range g.Hosts {
		if h == target {
			return
		}
	}
	g.Hosts = append(g.Hosts, target)
}

// RemoveHost drops a peer ssh target.
func (g *Registry) RemoveHost(target string) {
	kept := make([]string, 0, len(g.Hosts))
	for _, h := range g.Hosts {
		if h != target {
			kept = append(kept, h)
		}
	}
	g.Hosts = kept
}

// Load reads the self/hosts identity from state.json, returning a zero Registry
// when the file does not yet exist.
func Load() (*Registry, error) {
	raw, err := readRaw()
	if err != nil {
		return nil, err
	}
	g := &Registry{}
	if self, ok := raw["self"]; ok {
		if err := json.Unmarshal(self, &g.Self); err != nil {
			return nil, fmt.Errorf("parse self: %w", err)
		}
	}
	if hosts, ok := raw["hosts"]; ok {
		if err := json.Unmarshal(hosts, &g.Hosts); err != nil {
			return nil, fmt.Errorf("parse hosts: %w", err)
		}
	}
	return g, nil
}

// Update runs fn against the freshly loaded Registry under the shared reconcile
// lock, then writes back only the self+hosts keys — every other key already in
// state.json (repos, settings, default_location) is preserved byte-for-byte.
// Serializes read-modify-write across processes.
func Update(ctx context.Context, fn func(*Registry) error) (*Registry, error) {
	var out *Registry
	err := WithLock(ctx, func() error {
		raw, err := readRaw()
		if err != nil {
			return err
		}
		g := &Registry{}
		if self, ok := raw["self"]; ok {
			if err := json.Unmarshal(self, &g.Self); err != nil {
				return fmt.Errorf("parse self: %w", err)
			}
		}
		if hosts, ok := raw["hosts"]; ok {
			if err := json.Unmarshal(hosts, &g.Hosts); err != nil {
				return fmt.Errorf("parse hosts: %w", err)
			}
		}
		if err := fn(g); err != nil {
			return err
		}
		selfJSON, err := json.Marshal(g.Self)
		if err != nil {
			return fmt.Errorf("encode self: %w", err)
		}
		hostsJSON, err := json.Marshal(g.Hosts)
		if err != nil {
			return fmt.Errorf("encode hosts: %w", err)
		}
		raw["self"] = selfJSON
		raw["hosts"] = hostsJSON
		encoded, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("encode state: %w", err)
		}
		if err := save(append(encoded, '\n')); err != nil {
			return err
		}
		out = g
		return nil
	})
	return out, err
}

// DetectSelf returns the ssh target by which a peer reaches this machine,
// derived from the tailscale node name and the local user.
func DetectSelf(ctx context.Context, r Runner) (string, error) {
	out, err := r.Local(ctx, "tailscale", "status", "--json")
	if err != nil {
		return "", fmt.Errorf("detect self via tailscale (pass --self to override): %w", err)
	}
	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return "", fmt.Errorf("parse tailscale status (pass --self to override): %w", err)
	}
	node := TailscaleNode(status.Self.DNSName)
	if node == "" {
		return "", fmt.Errorf("empty tailscale node name (pass --self to override)")
	}
	user, err := r.Local(ctx, "id", "-un")
	if err != nil {
		return "", fmt.Errorf("detect local user: %w", err)
	}
	return strings.TrimSpace(user) + "@" + node, nil
}

// RemoveHost unregisters target as a peer and persists the change.
func RemoveHost(ctx context.Context, target string) error {
	if _, err := Update(ctx, func(g *Registry) error {
		g.RemoveHost(target)
		return nil
	}); err != nil {
		return fmt.Errorf("save state after removing %s: %w", target, err)
	}
	return nil
}

// VerifyResult reports a single host's reachability and reposync install state.
type VerifyResult struct {
	Target       string
	Reachable    bool
	Bootstrapped bool
	Version      string
	Err          error
}

// Verify probes target over ssh: whether it is reachable, has reposync installed,
// and its version.
func Verify(ctx context.Context, r Runner, target string) VerifyResult {
	res := VerifyResult{Target: target}
	if RemoteInstalled(ctx, r, target) {
		res.Reachable = true
		res.Bootstrapped = true
		if out, err := r.SSH(ctx, target, "reposync --version"); err == nil {
			res.Version = strings.TrimSpace(out)
		}
		return res
	}
	if _, err := r.SSH(ctx, target, "true"); err != nil {
		res.Err = fmt.Errorf("probe %s: %w", target, err)
		return res
	}
	res.Reachable = true
	return res
}

// VerifyAll verifies every host concurrently, returning one result per host in
// input order.
func VerifyAll(ctx context.Context, r Runner, hosts []string) []VerifyResult {
	results := make([]VerifyResult, len(hosts))
	sem := make(chan struct{}, maxConcurrentHosts)
	var wg sync.WaitGroup
	for i, target := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, target string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = Verify(ctx, r, target)
		}(i, target)
	}
	wg.Wait()
	return results
}

// RemoteInstalled reports whether reposync is on target's PATH over ssh.
func RemoteInstalled(ctx context.Context, r Runner, target string) bool {
	out, err := r.SSH(ctx, target, "command -v reposync")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// EachHost runs fn against every host concurrently (bounded), joining per-host
// failures into one error so a single down host never aborts the others.
func EachHost(ctx context.Context, hosts []string, fn func(ctx context.Context, target string) error) error {
	sem := make(chan struct{}, maxConcurrentHosts)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, target := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(target string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, target); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", target, err))
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%d host(s) failed: %w", len(errs), errors.Join(errs...))
}

// TailscaleNode returns the first DNS label of a tailscale DNSName.
func TailscaleNode(dnsName string) string {
	trimmed := strings.TrimSuffix(dnsName, ".")
	label, _, _ := strings.Cut(trimmed, ".")
	return label
}
