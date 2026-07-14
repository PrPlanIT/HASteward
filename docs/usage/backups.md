# Backup Model

Backups are stored in restic repositories with content-defined chunking (CDC) for block-level deduplication. All backups go through `restic backup --stdin` with a virtual filename in the snapshot.

## Snapshot Tags

Every snapshot is tagged with:

| Tag | Values | Description |
|-----|--------|-------------|
| `engine` | `cnpg`, `galera` | Database engine |
| `cluster` | cluster name | Database cluster CR name |
| `namespace` | namespace | Kubernetes namespace |
| `type` | `backup`, `diverged` | Snapshot type (see below) |
| `job` | `20060102T150405Z` | Groups diverged snapshots from the same repair (diverged only) |

## Snapshot Types

| Type | When | Virtual Path | Description |
|------|------|-------------|-------------|
| `backup` | Normal backup or pre-repair escrow | `<ns>/<cluster>/pgdumpall.sql` | Standard database dump. Escrow backups before repair are also `type=backup` and follow normal retention. |
| `diverged` | Split-brain detected during repair | `<ns>/<cluster>/<ordinal>-pgdumpall.sql` | Per-instance capture of each diverged replica. Shared `job` tag groups them. Forensic record for admin review. |

Engine-specific filenames: CNPG uses `pgdumpall.sql`, Galera uses `mysqldump.sql`.

## Snapshot Timestamps

All snapshots use `--time <job-start>` so the restic timestamp reflects when the operation was initiated, not when the dump completed. This ensures all snapshots from the same job (escrow + diverged captures) share a consistent timestamp.

## Retention / Prune

```bash
hasteward prune backups -e cnpg -c my-postgres -n my-ns --backups-path /backups
```

| Flag | Default | Description |
|------|---------|-------------|
| `--keep-last` | 7 | Keep the last N snapshots (or jobs for diverged) |
| `--keep-daily` | 30 | Keep N daily snapshots (or jobs for diverged) |
| `--keep-weekly` | 12 | Keep N weekly snapshots (or jobs for diverged) |
| `--keep-monthly` | 24 | Keep N monthly snapshots (or jobs for diverged) |
| `-t` / `--type` | `backup` | Snapshot type to prune: `backup`, `diverged`, or `all` |

**Group-aware prune for diverged snapshots**: Retention policies apply to job groups, not individual snapshots. A repair job that captured 3 diverged instances counts as 1 unit for `--keep-last`. Snapshots sharing the same `job` tag are kept or removed together.
