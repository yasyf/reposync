# reposync

![reposync banner](docs/assets/readme-banner.webp)

[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/reposync/ci.yml?branch=main&label=CI)](https://github.com/yasyf/reposync/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/reposync/blob/main/LICENSE)

Keep git repos in sync across your remote hosts.

reposync is a single Go binary that fetches, fast-forwards, and pushes a list of
git repositories on a timer. Point it at the repos you work on from more than one
machine, run `reposync install`, and a launchd agent keeps each one current with
its remote. There is no daemon to babysit and no shell cron to hand-edit.

## Install

```sh
brew install yasyf/tap/reposync
```

## Quickstart

```sh
# Write a starter config, then edit it to list your repos.
reposync config init

# Sync every configured repo once and see what changed.
reposync sync
# ✓ /Users/you/Code/dotfiles: pulled
# ✓ /Users/you/Code/notes: up-to-date

# Install the launchd agent to sync on the configured interval.
reposync install
# installed launchd agent at ~/Library/LaunchAgents/com.github.yasyf.reposync.plist (every 5m0s)
```

A repo entry needs a `path`. You can also set `remote`, which defaults to
`origin`; `branch`, which defaults to the current branch; and `auto_commit`, which
commits a dirty working tree before syncing:

```yaml
interval: 5m
repos:
  - path: ~/Code/dotfiles
    auto_commit: true
  - path: ~/Code/notes
    remote: origin
    branch: main
```

## What problems does this solve?

- Copies of the same repo drift apart. A laptop, a desktop, and a remote box each
  hold their own state, and reposync fast-forwards each from its remote so they
  converge instead of diverging.
- Wiring up cron or launchd by hand is fiddly. `reposync install` writes and loads
  the launchd plist for you, pointed at your config and set to your interval, and
  `reposync uninstall` removes it cleanly.
- Silent divergence is dangerous. When a branch has both local and remote commits,
  reposync refuses to guess. It reports the repo as diverged and leaves it
  untouched for you to merge.
- Working-tree changes get stranded. Set `auto_commit` on the repos where every
  local edit should be committed and pushed before a forgotten checkout on another
  host can lose it.

## License

PolyForm-Noncommercial-1.0.0. See [LICENSE](https://github.com/yasyf/reposync/blob/main/LICENSE).
