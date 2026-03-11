<p align="center">
  <img src="src/assets/logo-192.png" width="192" alt="HASteward">
</p>

# HASteward

**H**igh **A**vailability **Steward** is a (*WIP*) Go CLI and Kubernetes operator for database cluster triage, repair, backup, and restore. Pronounced like **Haste·Ward** or **H.A.** or **Ha!** Steward — flexible pronunciation. Backups use [restic](https://restic.net/) for block-level dedup, encryption, and compression.

<!-- sf:project:start -->
[![badge/GitHub-source-181717?logo=github](https://img.shields.io/badge/GitHub-source-181717?logo=github)](https://github.com/prplanit/HASteward) [![badge/GitLab-source-FC6D26?logo=gitlab](https://img.shields.io/badge/GitLab-source-FC6D26?logo=gitlab)](https://gitlab.prplanit.com/precisionplanit/hasteward) [![Go Report Card](https://goreportcard.com/badge/github.com/prplanit/HASteward)](https://goreportcard.com/report/github.com/prplanit/HASteward) [![Last Commit](https://img.shields.io/github/last-commit/prplanit/HASteward)](https://github.com/prplanit/HASteward/commits) [![Open Issues](https://img.shields.io/github/issues/prplanit/HASteward)](https://github.com/prplanit/HASteward/issues) ![github/issues-pr/prplanit/HASteward](https://img.shields.io/github/issues-pr/prplanit/HASteward) [![Contributors](https://img.shields.io/github/contributors/prplanit/HASteward)](https://github.com/prplanit/HASteward/graphs/contributors)
<!-- sf:project:end -->
<!-- sf:badges:start -->
[![build](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/build.svg)](https://gitlab.prplanit.com/precisionplanit/hasteward/-/pipelines) [![license](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/license.svg)](https://github.com/prplanit/HASteward/blob/main/LICENSE) [![release](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/release.svg)](https://github.com/prplanit/HASteward/releases) ![updated](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/updated.svg) [![badge/donate-FF5E5B?logo=ko-fi&logoColor=white](https://img.shields.io/badge/donate-FF5E5B?logo=ko-fi&logoColor=white)](https://ko-fi.com/T6T41IT163) [![badge/sponsor-EA4AAA?logo=githubsponsors&logoColor=white](https://img.shields.io/badge/sponsor-EA4AAA?logo=githubsponsors&logoColor=white)](https://github.com/sponsors/prplanit)
<!-- sf:badges:end -->
<!-- sf:image:start -->
[![badge/Docker-prplanit%2Fhasteward-2496ED?logo=docker&logoColor=white](https://img.shields.io/badge/Docker-prplanit%2Fhasteward-2496ED?logo=docker&logoColor=white)](https://hub.docker.com/r/prplanit/hasteward) [![pulls](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/pulls.svg)](https://hub.docker.com/r/prplanit/hasteward)

[![latest](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/latest.svg)](https://hub.docker.com/r/prplanit/hasteward/tags?name=latest) ![updated](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/release-updated.svg) [![size](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/release-size.svg)](https://hub.docker.com/r/prplanit/hasteward/tags?name=v0.0.1) [![latest-dev](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/latest-dev.svg)](https://hub.docker.com/r/prplanit/hasteward/tags?name=latest-dev) ![updated](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/dev-updated.svg) [![size](https://raw.githubusercontent.com/prplanit/HASteward/main/.stagefreight/badges/dev-size.svg)](https://hub.docker.com/r/prplanit/hasteward/tags?name=latest-dev)
<!-- sf:image:end -->

### Supported Engines

| Engine | Database | Operator |
|--------|----------|----------|
| `cnpg` | PostgreSQL | [CloudNativePG](https://cloudnative-pg.io/) |
| `galera` | MariaDB | [mariadb-operator](https://github.com/mariadb-operator/mariadb-operator) |

### Features

|                                    |                                                                                                       |
| ---------------------------------- | ----------------------------------------------------------------------------------------------------- |
| **Triage**                         | Read-only diagnostics: `pg_controldata`, `grastate.dat`, replication status, disk usage, split-brain   |
| **Repair**                         | Automated heal with pre-repair escrow backup, split-brain forensic capture, and safety gates           |
| **Backup / Restore**              | Streaming dump through `restic backup --stdin` — no temp files on database pods                        |
| **Retention**                      | Restic-style tag retention with group-aware diverged snapshot pruning                                  |
| **Operator Mode**                  | CRD-driven scheduler watches database CRs and runs triage/repair/backup on cron                       |
| **Bootstrap**                      | Full Galera cluster recovery from total failure with dry-run preview                                   |
| **WAL Prune**                      | Emergency CNPG WAL cleanup for disk-full deadlock recovery                                             |
| **Machine Output**                 | `--output json\|jsonl` for automation with typed envelopes, JSONL events, and `--dry-run` support      |

### Documentation

| Topic | |
|-------|-|
| [CLI Reference](docs/reference/CLI.md) | Subcommands and global flags |
| [Examples](docs/Examples.md) | CLI usage examples for every subcommand |
| [Backups](docs/Backups.md) | Backup model, snapshot tags, retention |
| [Safety Gates](docs/SafetyGates.md) | Repair and bootstrap safety matrices |
| [Operator Mode](docs/Operator.md) | CRDs, annotations, scheduler |
| [Architecture](docs/Architecture.md) | Engine repair flows, backup streaming |

### Templates

| File | |
|------|-|
| [Kubernetes Job](docs/k8s/job.yaml) | Ad-hoc Job manifest for CLI operations |

## License

Distributed under the [AGPL-3.0-only](LICENSE) License. See [LICENSING.md](docs/LICENSING.md) for commercial licensing.
