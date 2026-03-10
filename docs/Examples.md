# Examples

## Triage

```bash
hasteward triage -e cnpg -c zitadel-postgres -n zeldas-lullaby
```

## Repair All Unhealthy Replicas

```bash
hasteward repair -e galera -c osticket-mariadb -n hyrule-castle \
  --backups-path /backups
```

## Repair a Specific Instance

```bash
hasteward repair -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  -i 3 --backups-path /backups
```

## Force Repair During Split-Brain

```bash
hasteward repair -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  -i 2 --force --backups-path /backups
```

## Backup (Dump)

```bash
hasteward backup -e cnpg -c grafana-postgres -n gossip-stone \
  --backups-path /backups
```

## Backup (Native S3 — CNPG Only)

```bash
hasteward backup -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  --method native
```

## Restore from Latest Snapshot

```bash
hasteward restore -e galera -c osticket-mariadb -n hyrule-castle \
  --backups-path /backups
```

## Restore a Specific Snapshot

```bash
hasteward restore -e cnpg -c grafana-postgres -n gossip-stone \
  --backups-path /backups --snapshot abc123
```

## Restore from a Diverged Snapshot

```bash
hasteward restore -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  --backups-path /backups --snapshot abc123 -i 2
```

## Bootstrap a Dead Galera Cluster

```bash
# Preview the plan first
hasteward bootstrap -e galera -c kimai-mariadb -n hyrule-castle --dry-run --output json

# Execute
hasteward bootstrap -e galera -c kimai-mariadb -n hyrule-castle
```

## List Backups

```bash
hasteward get backups                              # all types
hasteward get backups -t diverged                  # diverged only
hasteward get backups -n zeldas-lullaby            # filter by namespace
hasteward get backups -c zitadel-postgres          # filter by cluster
```

## Export a Snapshot to File

```bash
hasteward export -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  --backups-path /backups --snapshot latest -o dump.sql.gz

# Export a diverged instance
hasteward export -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  --backups-path /backups --snapshot abc123 -i 2 -o instance2.sql.gz
```

## Prune Old Snapshots

```bash
hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  --backups-path /backups --keep-last 7 --keep-daily 30

# Prune diverged snapshots (group-aware: keeps last 3 repair jobs)
hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby \
  --backups-path /backups -t diverged --keep-last 3
```
