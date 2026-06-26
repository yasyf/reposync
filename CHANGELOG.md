# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/yasyf/reposync/compare/v0.9.0...HEAD
[0.9.0]: https://github.com/yasyf/reposync/releases/tag/v0.9.0
[0.8.1]: https://github.com/yasyf/reposync/releases/tag/v0.8.1
[0.8.0]: https://github.com/yasyf/reposync/releases/tag/v0.8.0
