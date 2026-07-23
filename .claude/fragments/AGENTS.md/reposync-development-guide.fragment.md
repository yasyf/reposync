# reposync Development Guide

Keep git repos in sync across your remote hosts.

## Repository Structure

```
reposync/
├── main.go              # Entrypoint; injects the release version into the CLI
├── internal/
│   ├── state/           # Reposync-owned keys of the shared JSON state at ~/.config/reposync: CRDT repo registries, settings, default location; synckit's hostregistry owns self/hosts and the flock
│   ├── vcs/             # Verified jj+git layer: detect, in-use, trunk, safe-advance, fast-forward trunk push, clone, watch paths
│   ├── sync/            # Idle-safe per-repo fetch + fast-forward; never clobbers; pushes trunk back only as a clean fast-forward once quiet past PushAfter
│   ├── reconcile/       # Pull-merge peer repo registries (synckit converge), clone-if-missing (temp→rename), idle-sync; per-host flock
│   ├── discover/        # Read-only scans: git/jj repos under default_location; network host candidates (Tailscale, Bonjour) via synckit's hostregistry
│   ├── apply/           # Batched repo enable/disable: one locked registry mutation, then a single reconcile of the newly enabled
│   ├── tui/             # Repos screen wired into the shared synckit TUI (tab router, header, built-in Hosts tab)
│   └── cli/             # Cobra wiring: root/tui, repo, host, self, sync, rpc-serve (stdio syncservice contract for the external synckitd watch daemon), install/uninstall (its manifest)
├── .goreleaser.yaml     # Cross-platform build + Homebrew cask release
├── .github/workflows/   # ci.yml (vet/test/build + golangci-lint) and release.yml (goreleaser)
├── docs/assets/         # Mascot logo, README banner, social-preview card
├── AGENTS.md            # This file — shared conventions
└── README.md            # Project overview
```
