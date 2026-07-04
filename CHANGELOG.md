# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.13.0] - 2026-07-03

### Changed
- Reconcile logs an unreachable peer once per outage — one line when the peer goes down and one with the outage duration when it recovers — instead of warning on every pass.
- Bump synckit to v0.8.0: tailscale host discovery now mints MagicDNS FQDN ssh targets (re-add hosts to adopt), and reconcile gains the per-outage peer logging above.

## [0.12.0] - 2026-07-03

### Fixed
- The TUI no longer renders past the bottom of the terminal, where long repo names used to push
  the end of the list and the key-hint bar off-screen. Bumping synckit to v0.7.2 stops the
  master-detail panes from re-wrapping rows already truncated to the column width and makes the
  router reserve the help bar's true height when `?` expands it. On the Repos tab, an open disable confirm or the
  applying spinner is now reserved out of the split instead of overflowing it, and repo keys are
  ignored while an apply is in flight so a stray enter cannot race the running apply.
- Canceling a git/jj invocation now sends SIGTERM to the whole process group, so the git that jj
  spawns for fetch and push also unwinds its ref transaction and unlinks its lock files before the
  10-second SIGKILL backstop fires.
- Sync self-heals a stale `packed-refs.lock` after 30 minutes, and reclaims a stale jj working-copy
  or git-import lock only once a flock probe confirms its holder is dead, instead of reporting the
  repo busy forever after a killed process left one behind.

### Changed
- reposync-driven git — including the git that jj spawns for fetch and push — runs with auto-gc and
  auto-maintenance suppressed, so no invocation holds `packed-refs.lock` inside a killable window.
- The default `repo_op_timeout` is raised from 2m to 5m.

## [0.10.2] - 2026-06-27

### Changed
- Bump synckit to v0.4.2: the shared Hosts tab seeds the registered mesh instantly and revalidates
  in place instead of showing a full loading screen on every launch.

## [0.10.1] - 2026-06-27

### Changed
- Bump synckit to v0.4.1: the shared Hosts tab now sorts registered mesh peers above discovered
  candidates.

## [0.10.0] - 2026-06-27

### Changed
- Adopt synckit v0.4.0's shared `tui` package. The terminal-UI shell and the Hosts tab now live in
  synckit (so every consumer shares them); reposync keeps only its Repos content screen, refactored
  to implement the exported `tui.Screen` on the shared primitives. The synckit host-discovery fixes
  ride along: the local Mac no longer lists itself in Hosts, and a peer that has the daemon installed
  reads "installed" instead of "reachable, not installed".

## [0.9.0] - 2026-06-26

### Changed
- Adopt synckit v0.3.0's typed RPC contract. reposync now serves synckitd over a single
  `reposync rpc-serve` stdio entrypoint (`syncservice.SyncConsumer`:
  capabilities/list/reconcile/sync/get_state) instead of the `list --json` / `reconcile` /
  `state get-json` CLI verbs. Cross-host registry fetch goes through typed `svc.get_state`
  over ssh-stdio; the apply stays a native in-process write (pull-only).

### Removed
- The `list`, `reconcile`, and `state` (get-json/apply-json) CLI commands, and the manifest's
  `actions` + `watch.list_cmd` — replaced by a `service{transport:"stdio",serve_args}` block.

## [0.8.1] - 2026-06-25

### Changed
- Release CI: use the shared `release-go.yml`; standardize on `HOMEBREW_TAP_TOKEN`.

## [0.8.0] - 2026-06-25

### Changed
- Adopt the `synckitd` daemon from github.com/yasyf/synckit v0.2.0. reposync is now a
  declarative consumer driven by synckitd through a manifest + CLI action contract.

### Added
- `reposync list --json` and `reposync state apply-json`; `reconcile --origin`.

### Removed
- The built-in watch loop, inline rpc server, host-bootstrap orchestration, and per-tool
  launchd. `host ls --json` shims to `synckitd host ls`; the peer mesh is read from the
  shared `~/.config/synckit`.

[Unreleased]: https://github.com/yasyf/reposync/compare/v0.10.2...HEAD
[0.10.2]: https://github.com/yasyf/reposync/releases/tag/v0.10.2
[0.10.1]: https://github.com/yasyf/reposync/releases/tag/v0.10.1
[0.10.0]: https://github.com/yasyf/reposync/releases/tag/v0.10.0
[0.9.0]: https://github.com/yasyf/reposync/releases/tag/v0.9.0
[0.8.1]: https://github.com/yasyf/reposync/releases/tag/v0.8.1
[0.8.0]: https://github.com/yasyf/reposync/releases/tag/v0.8.0
