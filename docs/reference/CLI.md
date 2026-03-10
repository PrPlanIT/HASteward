# CLI Reference

## Subcommands

| Command | Description |
|---------|-------------|
| `triage` | Read-only diagnostics for a database cluster |
| `repair` | Heal unhealthy database instances (with pre-repair backup) |
| `backup` | Back up a database cluster to a restic repository |
| `restore` | Restore a database cluster from a restic snapshot |
| `bootstrap` | Bootstrap a fully-down Galera cluster from the best candidate |
| `prune backups` | Apply retention policy and remove old snapshots |
| `prune wal` | Clear accumulated WAL from a disk-full CNPG instance |
| `get backups` | List restic backup snapshots |
| `get policies` | List BackupPolicy resources |
| `get repositories` | List BackupRepository resources |
| `get status` | Show triage status of managed database clusters |
| `export` | Extract a backup snapshot to a local `.sql.gz` file |
| `serve` | Run the operator (controller + scheduler) |

## Global Flags

| Flag | Short | Env | Description |
|------|-------|-----|-------------|
| `--engine` | `-e` | `HASTEWARD_ENGINE` | Database engine: `cnpg` or `galera` |
| `--cluster` | `-c` | `HASTEWARD_CLUSTER` | Database cluster CR name |
| `--namespace` | `-n` | `HASTEWARD_NAMESPACE` | Kubernetes namespace |
| `--backups-path` | | `HASTEWARD_BACKUPS_PATH` | Restic repository path or URL |
| `--restic-password` | | `RESTIC_PASSWORD` | Restic repository encryption password |
| `--instance` | `-i` | `HASTEWARD_INSTANCE` | Target specific instance number |
| `--force` | `-f` | `HASTEWARD_FORCE` | Override safety checks (targeted repair only) |
| `--no-escrow` | | `HASTEWARD_NO_ESCROW` | Skip pre-repair backup |
| `--method` | `-m` | `HASTEWARD_BACKUP_METHOD` | Backup method: `dump` (default) or `native` |
| `--snapshot` | | `HASTEWARD_SNAPSHOT` | Restic snapshot ID or `latest` (for restore) |
| `--heal-timeout` | | `HASTEWARD_HEAL_TIMEOUT` | Heal wait timeout in seconds (default: 600) |
| `--delete-timeout` | | `HASTEWARD_DELETE_TIMEOUT` | Delete wait timeout in seconds (default: 300) |
| `--output` | | `HASTEWARD_OUTPUT` | Output format: `auto`, `human`, `json`, `jsonl` |
| `--dry-run` | | | Show planned actions without executing |
| `--verbose` | `-v` | `HASTEWARD_VERBOSE` | Debug logging |
