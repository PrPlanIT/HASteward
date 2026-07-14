# Architecture

## CNPG Repair Flow

1. **Triage** — Collect `pg_controldata` from all instances, query replication status, check disk space
2. **Safety gate** — Verify primary is running, check for split-brain via timeline analysis
3. **Escrow** — Stream `pg_dumpall` from primary through `restic backup --stdin` (`type=backup`)
4. **Diverged** — If split-brain: dump each running instance individually (`type=diverged`, ordinal-prefixed, shared `job` tag)
5. **Heal** — Fence instance, clear pgdata on existing PVC, `pg_basebackup` from primary, unfence
6. **Re-triage** — Verify cluster health post-repair

## Galera Repair Flow

1. **Triage** — Read `grastate.dat` from all nodes, query `wsrep` status, check disk space
2. **Safety gate** — Verify healthy donor exists, check for split-brain via UUID/seqno comparison
3. **Escrow** — Stream `mysqldump` from healthy donor through `restic backup --stdin` (`type=backup`)
4. **Diverged** — If split-brain: dump each running instance individually (`type=diverged`, ordinal-prefixed, shared `job` tag)
5. **Heal** — Suspend CR, scale down, preserve and reset `grastate.dat`/`galera.cache`, scale up, resume
6. **Re-triage** — Verify cluster health post-repair

## Backup Streaming

Backups use Kubernetes exec API to pipe database dump output directly through `restic backup --stdin`. No intermediate storage or temporary files on the database pods. Restic handles chunking, dedup, encryption, and compression.
