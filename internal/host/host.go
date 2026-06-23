// Package host drives reposync's cross-host bootstrap: registering a peer,
// installing reposync on it over ssh, sharing repos, and converging remote
// reconcile/install. The repo-agnostic host-identity primitives (the Runner,
// DetectSelf, Verify, the self/hosts registry) live in the public
// github.com/yasyf/synckit/hostregistry package; this package aliases them and
// layers the reposync-specific orchestration on top, driving them through the
// reposync-named state.Config.
package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/yasyf/synckit/hostregistry"

	"github.com/yasyf/reposync/internal/state"
)

// Runner executes commands locally and over SSH; the SSH/exec boundary tests mock.
type Runner = hostregistry.Runner

// VerifyResult reports a single host's reachability and reposync install state.
type VerifyResult = hostregistry.VerifyResult

// NewExecRunner returns the default Runner that executes commands locally and over ssh.
func NewExecRunner() Runner { return hostregistry.NewExecRunner() }

// ShellQuote single-quotes s so it survives intact as one argument to a remote shell.
func ShellQuote(s string) string { return hostregistry.ShellQuote(s) }

// TailscaleNode returns the first DNS label of a tailscale DNSName.
func TailscaleNode(dnsName string) string { return hostregistry.TailscaleNode(dnsName) }

// DetectSelf returns the ssh target by which a peer reaches this machine.
func DetectSelf(ctx context.Context, r Runner) (string, error) {
	return hostregistry.DetectSelf(ctx, r)
}

// Verify probes target over ssh: reachability, reposync install, and version.
func Verify(ctx context.Context, r Runner, target string) VerifyResult {
	return state.Config.Verify(ctx, r, target)
}

// VerifyAll verifies every host concurrently, returning one result per host in input order.
func VerifyAll(ctx context.Context, r Runner, hosts []string) []VerifyResult {
	return state.Config.VerifyAll(ctx, r, hosts)
}

// RemoveHost unregisters target as a peer and persists the change.
func RemoveHost(ctx context.Context, target string) error {
	return state.Config.RemoveHost(ctx, target)
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

	// Resolve how peers reach this machine before persisting so state.Self is
	// recorded on both ends of a bootstrap. It is required on the primary path
	// (the inverse registration carries it) but best-effort on the no-recurse
	// path, where a peer may not run tailscale.
	if self == "" {
		detected, err := hostregistry.DetectSelf(ctx, r)
		if err != nil && !noRecurse {
			return log, err
		}
		self = detected // "" when detection fails on the no-recurse path
	}

	if _, err := state.Config.Update(ctx, func(g *hostregistry.Registry) error {
		g.UpsertHost(target)
		if self != "" {
			g.Self = self
		}
		return nil
	}); err != nil {
		return log, fmt.Errorf("save state after registering %s: %w", target, err)
	}
	step("registered host " + target + " in local state")
	if self != "" {
		step("self identity: " + self)
	}

	if noRecurse {
		step("no-recurse: skipping remote bootstrap")
		return log, nil
	}

	if state.Config.RemoteInstalled(ctx, r, target) {
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

// PropagateRepo upserts repo onto every registered peer via repo add-remote,
// skipping local-only or remoteless repos.
func PropagateRepo(ctx context.Context, st *state.State, r Runner, repo state.Repo) error {
	if repo.LocalOnly || repo.Origin == "" {
		return nil
	}
	cmd := addRemoteCmd(repo)
	return hostregistry.EachHost(ctx, st.Hosts, func(ctx context.Context, target string) error {
		_, err := r.SSH(ctx, target, cmd)
		return err
	})
}

// RemoteReconcile triggers a reconcile on every registered peer's resident daemon
// over its RPC socket; a down host is logged into the returned error and does not
// abort the others.
func RemoteReconcile(ctx context.Context, st *state.State, r Runner) error {
	return hostregistry.EachHost(ctx, st.Hosts, func(ctx context.Context, target string) error {
		_, err := r.SSH(ctx, target, "reposync rpc reconcile")
		return err
	})
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

func addRemoteCmd(repo state.Repo) string {
	return fmt.Sprintf(
		"reposync repo add-remote --origin %s --relpath %s --trunk %s",
		ShellQuote(repo.Origin), ShellQuote(repo.Relpath), ShellQuote(repo.Trunk),
	)
}

func isNoSuchCask(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no available") ||
		strings.Contains(m, "no cask") ||
		strings.Contains(m, "no formulae")
}
