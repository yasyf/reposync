// Package host handles cross-host registration and SSH bootstrap: detecting how
// peers reach this machine, installing reposync on a remote, registering the
// inverse host, sharing state, and driving remote reconcile/install over SSH.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/yasyf/reposync/internal/state"
)

const maxConcurrentHosts = 8

// Runner executes commands locally and over SSH; the SSH/exec boundary tests mock.
type Runner interface {
	// Local runs name with args on this machine and returns its stdout.
	Local(ctx context.Context, name string, args ...string) (string, error)
	// SSH runs remoteCmd on target over ssh and returns its stdout.
	SSH(ctx context.Context, target, remoteCmd string) (string, error)
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

// AddHost registers target as a peer and, unless noRecurse, SSH-bootstraps
// reposync on it: install if missing, register the inverse host, share state,
// then reconcile and install services. It returns a human-readable step log.
func AddHost(ctx context.Context, st *state.State, r Runner, target, self string, noRecurse bool) ([]string, error) {
	return AddHostStream(ctx, st, r, target, self, noRecurse, nil)
}

// AddHostStream is AddHost with live progress: onStep (may be nil) is called with
// each step as it happens.
func AddHostStream(ctx context.Context, st *state.State, r Runner, target, self string, noRecurse bool, onStep func(string)) ([]string, error) {
	var log []string
	step := func(msg string) {
		log = append(log, msg)
		if onStep != nil {
			onStep(msg)
		}
	}

	if _, err := state.Update(func(s *state.State) error {
		s.UpsertHost(target)
		return nil
	}); err != nil {
		return log, fmt.Errorf("save state after registering %s: %w", target, err)
	}
	step("registered host " + target + " in local state")

	if noRecurse {
		step("no-recurse: skipping remote bootstrap")
		return log, nil
	}

	if self == "" {
		detected, err := DetectSelf(ctx, r)
		if err != nil {
			return log, err
		}
		self = detected
	}
	step("self identity: " + self)

	if remoteInstalled(ctx, r, target) {
		step("reposync already installed on " + target)
	} else {
		if err := remoteBrewInstall(ctx, r, target); err != nil {
			return log, err
		}
		step("installed reposync on " + target + " via brew")
	}

	if _, err := r.SSH(ctx, target, "reposync host add "+self+" --no-recurse"); err != nil {
		return log, fmt.Errorf("register inverse host on %s: %w", target, err)
	}
	step("registered inverse host " + self + " on " + target)

	for _, repo := range st.Repos {
		if repo.LocalOnly || repo.Origin == "" {
			continue
		}
		if _, err := r.SSH(ctx, target, addRemoteCmd(repo)); err != nil {
			step(fmt.Sprintf("WARN share repo %s to %s: %v", repo.Relpath, target, err))
			continue
		}
		step("shared repo " + repo.Relpath + " to " + target)
	}

	if _, err := r.SSH(ctx, target, "reposync reconcile"); err != nil {
		step(fmt.Sprintf("WARN reconcile on %s: %v", target, err))
	} else {
		step("reconciled " + target)
	}

	if _, err := r.SSH(ctx, target, "reposync install"); err != nil {
		step(fmt.Sprintf("WARN install services on %s: %v", target, err))
	} else {
		step("installed services on " + target)
	}

	return log, nil
}

// RemoveHost unregisters target as a peer and persists the change.
func RemoveHost(target string) error {
	if _, err := state.Update(func(s *state.State) error {
		s.RemoveHost(target)
		return nil
	}); err != nil {
		return fmt.Errorf("save state after removing %s: %w", target, err)
	}
	return nil
}

// PropagateRepo upserts repo onto every registered peer via repo add-remote,
// skipping local-only or remoteless repos.
func PropagateRepo(ctx context.Context, st *state.State, r Runner, repo state.Repo) error {
	if repo.LocalOnly || repo.Origin == "" {
		return nil
	}
	cmd := addRemoteCmd(repo)
	return eachHost(ctx, st.Hosts, func(ctx context.Context, target string) error {
		_, err := r.SSH(ctx, target, cmd)
		return err
	})
}

// RemoteReconcile triggers a reconcile on every registered peer's resident daemon
// over its RPC socket; a down host is logged into the returned error and does not
// abort the others.
func RemoteReconcile(ctx context.Context, st *state.State, r Runner) error {
	return eachHost(ctx, st.Hosts, func(ctx context.Context, target string) error {
		_, err := r.SSH(ctx, target, "reposync rpc reconcile")
		return err
	})
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
	if remoteInstalled(ctx, r, target) {
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

func remoteInstalled(ctx context.Context, r Runner, target string) bool {
	out, err := r.SSH(ctx, target, "command -v reposync")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

func remoteBrewInstall(ctx context.Context, r Runner, target string) error {
	// brew trust is required when the remote sets HOMEBREW_REQUIRE_TAP_TRUST,
	// which blocks loading casks from third-party taps; it is idempotent and a
	// no-op otherwise.
	out, err := r.SSH(ctx, target, "brew tap yasyf/tap && brew trust yasyf/tap && brew install --cask yasyf/tap/reposync")
	if err == nil {
		return nil
	}
	if isNoSuchCask(out) || isNoSuchCask(err.Error()) {
		return fmt.Errorf("brew has no reposync cask yet on %s: publish a goreleaser release to yasyf/homebrew-tap first: %w", target, err)
	}
	return fmt.Errorf("brew install reposync on %s: %w", target, err)
}

func eachHost(ctx context.Context, hosts []string, fn func(ctx context.Context, target string) error) error {
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

func addRemoteCmd(repo state.Repo) string {
	return fmt.Sprintf(
		"reposync repo add-remote --origin %s --relpath %s --trunk %s",
		ShellQuote(repo.Origin), ShellQuote(repo.Relpath), ShellQuote(repo.Trunk),
	)
}

// TailscaleNode returns the first DNS label of a tailscale DNSName.
func TailscaleNode(dnsName string) string {
	trimmed := strings.TrimSuffix(dnsName, ".")
	label, _, _ := strings.Cut(trimmed, ".")
	return label
}

func isNoSuchCask(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no available") ||
		strings.Contains(m, "no cask") ||
		strings.Contains(m, "no formulae")
}

// ShellQuote single-quotes s so it survives intact as one argument to a remote
// shell, escaping any embedded single quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
