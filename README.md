# reposync

![reposync banner](docs/assets/readme-banner.webp)

[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/reposync/ci.yml?branch=main&label=CI)](https://github.com/yasyf/reposync/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/reposync/blob/main/LICENSE)

Keep git repos in sync across your remote hosts.

reposync is a single Go binary that keeps a set of repos converged across your
machines: present everywhere, kept on the latest `main`, and never clobbering
work in progress. Register your hosts and repos, and it clones each repo onto
every host that is missing it and fast-forwards it on a timer and on filesystem
events.

## Install

```sh
brew install yasyf/tap/reposync
```

## Quickstart

Register a peer host, then a repo. reposync reaches the peer over Tailscale,
installs itself there, shares your repo list, and clones each repo wherever it is
missing:

```sh
reposync host add yasyf@yasyf-home
reposync repo add ~/Code/cc-review
reposync install   # launchd: a 15-minute reconcile tick + a watch daemon
```

## Commands

| Command | What it does |
| --- | --- |
| `reposync host add <user@node>` | Register a peer and converge (`--local-only` repos stay put) |
| `reposync repo add <path>` | Track a repo relative to `default_location` (`~/Code`) and clone it on every peer |
| `reposync sync` | Idle-safe fetch + fast-forward of every repo |
| `reposync reconcile` | Clone any missing repo, then idle-sync the rest |
| `reposync install` / `uninstall` | Add or remove the launchd agents |
| `reposync repo ls` / `host ls` | Inspect what is registered |

Run `reposync --help` for the full command tree and flags.

## How convergence works

Each repo is tracked by its path relative to `default_location` (`~/Code`), so it
lands at the same place on every host. Clones run `jj git clone --colocate` — jj
is preferred, plain git fully supported. A sync is pull-only: `jj git fetch` (or
`git fetch`) plus a safe fast-forward, never a push, and it skips any repo that is
dirty, would not fast-forward, or was active within the idle threshold. A launchd
tick reconciles every 15 minutes so offline peers self-heal, and a watchman-backed
daemon notifies peers within seconds of a trunk change.

## Prerequisite for cross-host bootstrap

`reposync host add` installs the peer via `brew install --cask yasyf/tap/reposync`,
so cut a goreleaser release to `yasyf/homebrew-tap` before adding your first host;
until then `host add` fails fast and tells you to publish one.

## License

PolyForm-Noncommercial-1.0.0. See [LICENSE](https://github.com/yasyf/reposync/blob/main/LICENSE).
