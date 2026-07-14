# Safety Gates

## Repair Mode

| Scenario | Untargeted | Targeted |
|----------|-----------|----------|
| Split-brain detected | **HARD STOP** (no override) | Fail (override with `--force`) |
| Target is primary | N/A | **HARD STOP** (no override) |
| Target is healthy | Skipped automatically | Skip (override with `--force`) |
| No healthy donor | **ABORT** | **ABORT** |

## Pre-Repair Backup Behavior

| Configuration | Before Heal | Split-Brain |
|---------------|-------------|-------------|
| Default (`--backups-path`) | Escrow saved as `type=backup` | + Per-instance `type=diverged` with `job` tag |
| `--no-escrow` | Warning, no backup | No diverged captures |
| Heal fails | Escrow persists (normal backup retention) | Diverged snapshots persist for admin review |

## Bootstrap Safety (Galera Only)

| Scenario | Behavior |
|----------|----------|
| Any healthy nodes exist | **ABORT** — use `repair` instead |
| Ambiguous seqno across nodes | Fail (override with `--force`) |
| Split-brain detected | Fail (override with `--force`) |
| `--dry-run` | Preview decision + planned actions, no mutation |
