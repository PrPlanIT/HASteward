<p align="center">
  <img src="src/assets/logo-192.png" width="192" alt="HASteward">
</p>

# HASteward

**H**igh **A**vailability **Steward** is a (*WIP*) Go CLI and Kubernetes operator for database cluster triage, repair, backup, and restore. Pronounced like **Haste·Ward** or **H.A.** or **Ha!** Steward — flexible pronunciation. Backups use [restic](https://restic.net/) for block-level dedup, encryption, and compression.

<!-- sf:project:start -->
[![GitHub](https://img.shields.io/badge/GitHub-source-181717?logo=github)](https://github.com/PrPlanIT/HASteward) [![GitLab](https://img.shields.io/badge/GitLab-source-FC6D26?logo=gitlab)](https://gitlab.prplanit.com/PrPlanIT/hasteward) [![Go Version](https://img.shields.io/github/go-mod/go-version/PrPlanIT/HASteward?logo=go)](https://github.com/PrPlanIT/HASteward) [![Last Commit](https://img.shields.io/github/last-commit/PrPlanIT/HASteward)](https://github.com/PrPlanIT/HASteward/commits) [![Open Issues](https://img.shields.io/github/issues/PrPlanIT/HASteward)](https://github.com/PrPlanIT/HASteward/issues) [![Contributors](https://img.shields.io/github/contributors/PrPlanIT/HASteward)](https://github.com/PrPlanIT/HASteward/graphs/contributors)
<!-- sf:project:end -->
<!-- sf:badges:start -->
[![build](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/build.svg)](https://gitlab.prplanit.com/PrPlanIT/hasteward/-/pipelines) [![license](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/license.svg)](https://github.com/PrPlanIT/HASteward/blob/main/LICENSE) [![release](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/release.svg)](https://github.com/PrPlanIT/HASteward/releases) ![updated](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/updated.svg) [![donate](https://img.shields.io/badge/donate-FF5E5B?logo=ko-fi&logoColor=white)](https://ko-fi.com/T6T41IT163) [![sponsor](https://img.shields.io/badge/sponsor-EA4AAA?logo=githubsponsors&logoColor=white)](https://github.com/sponsors/PrPlanIT)
<!-- sf:badges:end -->
<!-- sf:image:start -->
[![GHCR](https://img.shields.io/badge/GHCR-prplanit%2Fhasteward-181717?logo=github&logoColor=white)](https://github.com/PrPlanIT/HASteward/pkgs/container/hasteward) [![Docker](https://img.shields.io/badge/Docker-prplanit%2Fhasteward-2496ED?logo=docker&logoColor=white)](https://hub.docker.com/r/prplanit/hasteward) [![pulls](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/pulls.svg)](https://hub.docker.com/r/prplanit/hasteward) [![Harbor](https://img.shields.io/badge/Harbor-prplanit%2Fhasteward-60b932)](https://cr.pcfae.com/harbor/projects)

[![latest](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/release-latest.svg)](https://github.com/PrPlanIT/HASteward/pkgs/container/hasteward) ![updated](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/release-updated.svg) [![size](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/release-size.svg)](https://github.com/PrPlanIT/HASteward/pkgs/container/hasteward) [![latest-dev](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/dev-latest.svg)](https://github.com/PrPlanIT/HASteward/pkgs/container/hasteward) ![updated](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/dev-updated.svg) [![size](https://raw.githubusercontent.com/PrPlanIT/HASteward/main/.stagefreight/scribe/dev-size.svg)](https://github.com/PrPlanIT/HASteward/pkgs/container/hasteward)
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
| [Container Usage](docs/ContainerUsage.md) | Run HASteward via full `docker run` commands against a cluster |
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
