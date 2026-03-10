<p align="center">
  <img src="src/assets/logo.png" width="220" alt="HASteward">
</p>

# HASteward

> *Your clusters, under careful watch*

**H**igh **A**vailability **Steward** — Go CLI and Kubernetes operator for database cluster triage, repair, backup, and restore. Pronounced **Haste·Ward** — warding against haste, not moving hastily. Backups use [restic](https://restic.net/) for block-level dedup, encryption, and compression.

<!-- sf:badges:start -->
<!-- sf:badges:end -->

### Public Resources

|                  |                                                                                          |
| ---------------- | ---------------------------------------------------------------------------------------- |
| Docker Images    | [Docker Hub](https://hub.docker.com/r/prplanit/hasteward)                                |
| Source Code      | [GitHub](https://github.com/prplanit/HASteward)                                         |

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

### Container Image

```
docker.io/prplanit/hasteward:latest
```

Single static binary with embedded restic. No runtime dependencies.

## License

Distributed under the [AGPL-3.0-only](LICENSE) License. See [LICENSING.md](docs/LICENSING.md) for commercial licensing.
