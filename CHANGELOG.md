# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.26.0] - 2026-07-24

### Changed
- Pin daemonkit v0.17.4 so resident runtime drain settles admitted requests and
  terminal transport acknowledgements before socket teardown.

## [0.25.0] - 2026-07-24

### Changed
- Pin daemonkit v0.17.2 and Synckit v0.33.0. Resident helpers now supply the
  dispatcher resolved for each exact admitted publication.

## [0.24.0] - 2026-07-23

### Changed
- Pin daemonkit v0.16.0 as the exact resident runtime dependency, including
  direct-parent proof before inherited spawned-session descriptors transfer.

## [0.23.0] - 2026-07-23

### Changed
- Pin daemonkit v0.14.0 and Synckit v0.31.0 as the exact resident runtime and
  revision-delivery dependencies.
- Replace the spawned stdio and product-owned SSH pull paths with one resident
  `rpc-serve-v1` service. Synckit now owns exact sessions and durable delivery;
  reposync exports and applies an immutable revisioned `reposync-transfer-v1`
  payload with base fencing, digest binding, and idempotent acknowledgements.
- Remove the custom registry and `env.get_state` peer methods. Repository CRDT
  state and `.env` state now converge through the same bounded snapshot/delta.

## [0.22.0] - 2026-07-23

### Changed
- Pin daemonkit v0.10.0 as the exact fleet runtime dependency.

## [0.21.1] - 2026-07-23

### Security
- Darwin binaries are now required to carry the expected Developer ID signature
  and notarization before release publication. Release configuration no longer
  disables signing when credentials are missing or strips quarantine after install.

## [0.21.0] - 2026-07-23

### Changed
- Pin Synckit v0.29.0 and its daemonkit v0.9.0 runtime as the exact fleet hard-cut dependencies.
- Environment sidecars now use one exact v1 identity, schema fingerprint, and
  closed field set; legacy or extended sidecars fail closed with no in-code importer.
- `rpc-serve` now uses Synckit's daemonkit-owned exact spawned-session transport,
  and installation emits only the strict current service manifest schema.

## [0.15.1] - 2026-07-14

### Fixed
- A background advance could sweep uncommitted files out of a colocated jj working copy: edits
  landing between reposync's disposability check and its own `jj new`/`jj rebase` were captured
  by the mutation's at-execution snapshot and stranded off-disk in an anonymous head. Advance now
  verifies every mutation after the fact, treating a surviving outgoing change after `jj new`, or
  rebase paths escaping the pre-classified generated set, as proof that live edits were swept. It
  recovers them automatically, forward onto the new trunk when conflict-free or back onto the
  original parents otherwise (`recovered`). When concurrent activity makes recovery unsafe, it
  goes hands-off with the content preserved in a visible commit (`swept`); sync logs both.

## [0.15.0] - 2026-07-14

### Added
- reposync now syncs untracked root `.env*` files across hosts, merged key by key: the newest
  edit wins per key, deletions propagate, and each host keeps its own comments and line order.
  The merge rides the reconcile pass over the existing rpc-serve ssh channel (a new lock-free
  `env.get_state` method), gates on a five-second quiet window so it never races a live edit,
  and leaves alone anything git tracks, symlinks, and files over 256 KiB. Per-repo merge state
  lives under `~/.config/reposync/env/`. Opt a repo out with `reposync repo add --no-env-sync`;
  the setting shows as an ENV column in `repo ls` and an env line in the TUI detail pane.
  Upgrade all hosts together: an older host answering `env.get_state` with an unknown-method
  error is skipped until upgraded, and re-serving the repo registry from an old binary can drop
  the `no_env_sync` flag on a same-microsecond registry tie.

## [0.14.0] - 2026-07-13

### Changed
- The synckit watch backend flips to hardened fsnotify.
- CI's test job resolves Go from go.mod like every other job.

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

## [0.11.1] - 2026-07-03

### Changed
- Bump synckit to v0.7.1, picking up the fix for a nil-pointer crash in the stale-exchange
  transport, where an abandoned exchange goroutine read pipe fields the transport had already
  reset after a converge peer timeout. The bump also pulls in the synckit v0.6.0/v0.7.0 line.

## [0.11.0] - 2026-07-02

### Fixed
- Sync never disrupts an active repo. reposync used to race a user's rapid raw `git commit`s in a
  jj-colocated repo and orphan the commits, because a raw commit moves git HEAD with no jj op, so
  the next snapshot reconciled against the diverged HEAD and stranded it. Every sync mutation is
  now gated on provable quiescence. A stat-only `opInProgress` probe of the git/jj lock and state
  markers, from `index.lock` and `packed-refs.lock` through merge/rebase/bisect state to jj's
  `working_copy.lock` and `git_import_export.lock`, runs before any shell-out, so a live operation
  short-circuits `InUse` instead of hanging on jj's working-copy lock; `Advance` captures git HEAD
  after the fetch and re-checks it immediately before each mutation, aborting with `OutcomeRaced`
  when the repo drifts underneath; and the default idle threshold is raised from 5m to 30m so a
  user committing every few minutes no longer falls inside the idle window.
- Reconcile reports a busy repo as busy instead of present, so a busy-gated idle-sync no longer
  counts as a completed one.
- The TUI reads the idle threshold from the same state load that discovers repos, instead of a
  dead 10-minute fallback that ignored the configured value.
- git `InUse` probes recent activity before the dirty-tree check, matching jj's gate order, so a
  recently active repo reports "recent activity" instead of "dirty working tree".
- git `Advance` classifies divergence structurally from the ahead/behind counts, so a trunk with
  local commits now reports diverged instead of masquerading as up to date, and fast-forward
  failures are returned as errors instead of swallowed.
- jj `Advance` classifies the working copy in a single probe, and the op-log scan pages in growing
  chunks so a burst of reposync's own noise ops can no longer bury the last real operation.

### Changed
- Bump synckit to v0.5.0: busy repos are signaled on the watch wire, and sync summaries carry
  skipped-busy counts.

## [0.10.3] - 2026-07-01

### Fixed
- Daemon probes are snapshot-free, so they never race an in-flight interactive jj command into
  `Internal error: Failed to check out commit: Concurrent checkout`. `InUse` runs the op-log
  recency gate first and reads the last-recorded `@` with `--ignore-working-copy`, `Advance`
  short-circuits before any snapshot when trunk has not moved, and residual working-copy
  contention degrades to a busy skip retried on the next converge.

### Changed
- The manifest watch debounce is raised to 15s so a converge fires after a push instead of
  mid-flight, and the never-consumed `WatchDebounce`/`Interval` settings knobs are dropped.

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

[Unreleased]: https://github.com/yasyf/reposync/compare/v0.24.0...HEAD
[0.24.0]: https://github.com/yasyf/reposync/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/yasyf/reposync/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/yasyf/reposync/compare/v0.21.1...v0.22.0
[0.21.1]: https://github.com/yasyf/reposync/compare/v0.21.0...v0.21.1
[0.21.0]: https://github.com/yasyf/reposync/compare/v0.15.1...v0.21.0
[0.15.1]: https://github.com/yasyf/reposync/compare/v0.15.0...v0.15.1
[0.15.0]: https://github.com/yasyf/reposync/releases/tag/v0.15.0
[0.14.0]: https://github.com/yasyf/reposync/releases/tag/v0.14.0
[0.13.0]: https://github.com/yasyf/reposync/releases/tag/v0.13.0
[0.12.0]: https://github.com/yasyf/reposync/releases/tag/v0.12.0
[0.11.1]: https://github.com/yasyf/reposync/releases/tag/v0.11.1
[0.11.0]: https://github.com/yasyf/reposync/releases/tag/v0.11.0
[0.10.3]: https://github.com/yasyf/reposync/releases/tag/v0.10.3
[0.10.2]: https://github.com/yasyf/reposync/releases/tag/v0.10.2
[0.10.1]: https://github.com/yasyf/reposync/releases/tag/v0.10.1
[0.10.0]: https://github.com/yasyf/reposync/releases/tag/v0.10.0
[0.9.0]: https://github.com/yasyf/reposync/releases/tag/v0.9.0
[0.8.1]: https://github.com/yasyf/reposync/releases/tag/v0.8.1
[0.8.0]: https://github.com/yasyf/reposync/releases/tag/v0.8.0
