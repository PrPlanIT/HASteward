# HASteward (Ansible Edition)

**H**igh **A**vailability **Steward** — Ansible playbook that standardizes recovery and backup logic for stateful database services in Kubernetes, safely reviving stalled replicas and managing backup lifecycles.

> **Note:** This is the legacy Ansible-based implementation. The current version is a Go CLI binary and Kubernetes operator at the repository root. This playbook remains functional and is documented here for reference.

## Supported Engines

| Engine | Database | Operator |
|--------|----------|----------|
| `cnpg` | PostgreSQL | [CloudNativePG](https://cloudnative-pg.io/) |
| `galera` | MariaDB | [mariadb-operator](https://github.com/mariadb-operator/mariadb-operator) |

## Modes

| Mode | Description |
|------|-------------|
| `triage` | Read-only diagnostic. Collects cluster state, analyzes all instances, displays assessment. **(default)** |
| `repair` | Triage, safety gate, escrow backup, heal unhealthy instances, re-triage. |
| `backup` | Take a backup of the cluster (logical dump or native S3). |
| `restore` | Restore a cluster from a backup file. |

## Parameters

### Required

| Parameter | Description |
|-----------|-------------|
| `engine` | Database engine: `cnpg` or `galera` |
| `cluster_name` | Name of the database CR (CNPG Cluster or MariaDB) |
| `namespace` | Kubernetes namespace |

### Mode Selection

| Parameter | Default | Description |
|-----------|---------|-------------|
| `mode` | `triage` | Operation mode: `triage`, `repair`, `backup`, `restore` |

### Repair Options

| Parameter | Default | Description |
|-----------|---------|-------------|
| `backups_path` | — | Local path for backup storage. **Required** unless `no_escrow=true`. |
| `instance_number` | — | Target a specific instance (e.g., `2`). Without this, heals all unhealthy. |
| `force` | `false` | Override split-brain/healthy safety checks (targeted repair only). |
| `no_escrow` | `false` | Skip escrow backup before repair. Explicit opt-in to accept the risk. |
| `retain_escrow` | `false` | Keep escrow after successful repair (otherwise deleted on success). |
| `heal_timeout` | `600` | Seconds to wait for a healed instance to become ready. |
| `delete_timeout` | `300` | Seconds to wait for a pod to terminate during heal. |

### Backup/Restore Options

| Parameter | Default | Description |
|-----------|---------|-------------|
| `backups_path` | — | Local path for backup storage. **Required** for dump method. |
| `backup_method` | `dump` | `dump` (logical SQL, works everywhere) or `native` (CNPG barmanObjectStore S3 only). |
| `backup_file` | — | Specific backup file to restore. Defaults to latest in `backups_path/<ns>/<cluster>/`. |

## Backup Directory Structure

```
<backups_path>/<namespace>/<cluster_name>/
  <ns>-<cluster>-<engine>-<datetime>-backup.sql.gz    # mode=backup output
  <ns>-<cluster>-<engine>-<datetime>-escrow.sql.gz    # mode=repair escrow (deleted on success)
```

Restore discovers both `-backup.sql.gz` and `-escrow.sql.gz` files, sorted by timestamp, using the latest by default.

## Usage

### Running Directly

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=my-postgres \
  -e namespace=my-namespace \
  -e mode=triage
```

### Running in Kubernetes

A Job template is provided in `hasteward-job.yaml` with three components:

```bash
# One-time setup: ServiceAccount + RBAC
kubectl apply -f hasteward-job.yaml -l app.kubernetes.io/component=rbac

# One-time setup: Backups PVC (static CephFS PV/PVC — edit paths for your cluster)
kubectl apply -f hasteward-job.yaml -l app.kubernetes.io/component=storage

# Per-run: Edit the Job args, then apply
kubectl apply -f hasteward-job.yaml -l app.kubernetes.io/component=job
```

The Job fetches playbook files from Flux source-controller and mounts a PVC for backup storage. Edit the `args:` section to set your parameters per run.

## Examples

### Triage a CNPG Cluster

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=zitadel-postgres \
  -e namespace=zeldas-lullaby \
  -e mode=triage
```

### Repair All Unhealthy Replicas (with Escrow)

```bash
ansible-playbook hasteward.yml \
  -e engine=galera \
  -e cluster_name=osticket-mariadb \
  -e namespace=hyrule-castle \
  -e mode=repair \
  -e backups_path=/backups
```

### Repair a Specific Instance

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=zitadel-postgres \
  -e namespace=zeldas-lullaby \
  -e mode=repair \
  -e instance_number=3 \
  -e backups_path=/backups
```

### Force Repair During Split-Brain

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=zitadel-postgres \
  -e namespace=zeldas-lullaby \
  -e mode=repair \
  -e instance_number=2 \
  -e force=true \
  -e backups_path=/backups
```

### Backup a Cluster (Dump)

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=grafana-postgres \
  -e namespace=gossip-stone \
  -e mode=backup \
  -e backups_path=/backups
```

### Backup a CNPG Cluster (Native S3)

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=zitadel-postgres \
  -e namespace=zeldas-lullaby \
  -e mode=backup \
  -e backup_method=native
```

### Restore from Latest Backup

```bash
ansible-playbook hasteward.yml \
  -e engine=galera \
  -e cluster_name=osticket-mariadb \
  -e namespace=hyrule-castle \
  -e mode=restore \
  -e backups_path=/backups
```

### Restore from Specific File

```bash
ansible-playbook hasteward.yml \
  -e engine=cnpg \
  -e cluster_name=grafana-postgres \
  -e namespace=gossip-stone \
  -e mode=restore \
  -e backups_path=/backups \
  -e backup_file=/backups/gossip-stone/grafana-postgres/gossip-stone-grafana-postgres-cnpg-20260211T120000-backup.sql.gz
```

## Safety Gates

### Repair Mode

| Scenario | Untargeted | Targeted |
|----------|-----------|----------|
| Split-brain detected | **HARD STOP** (no override) | Fail (override with `force=true`) |
| Target is primary | N/A | **HARD STOP** (no override) |
| Target is healthy | Skipped automatically | Skip (override with `force=true`) |
| No healthy donor | **ABORT** | **ABORT** |

### Escrow Behavior

| Configuration | Before Heal | After Successful Heal |
|---------------|-------------|----------------------|
| `backups_path=/backups` (default) | Escrow saved as `-escrow.sql.gz` | Escrow **deleted** |
| `backups_path=/backups -e retain_escrow=true` | Backup saved as `-backup.sql.gz` | Backup **retained** |
| `no_escrow=true` | Warning displayed, no backup | N/A |
| Heal **fails** | Escrow preserved | Escrow preserved (message shows path) |

## How It Works

### CNPG Repair Flow

1. **Triage** — Collect `pg_controldata` from all instances (exec on running pods, PVC probes for down instances), query replication status, check disk space
2. **Safety gate** — Verify primary is running, check for split-brain via timeline analysis
3. **Escrow** — Stream `pg_dumpall` from primary to gzipped file via Kubernetes exec API
4. **Heal** — Fence instance, clear pgdata on existing PVC, `pg_basebackup` from primary, unfence
5. **Re-triage** — Verify cluster health post-repair

### Galera Repair Flow

1. **Triage** — Read `grastate.dat` from all nodes, query `wsrep` status, check disk space
2. **Safety gate** — Verify healthy donor exists, check for split-brain via UUID/seqno comparison
3. **Escrow** — Stream `mysqldump` from healthy donor to gzipped file via Kubernetes exec API
4. **Heal** — Suspend CR, scale down, preserve and reset `grastate.dat`/`galera.cache`, scale up, resume
5. **Re-triage** — Verify cluster health post-repair

### Backup Streaming

Backups use the Python `kubernetes` client's streaming exec API to pipe database dump output directly to a gzipped file on the controller, with progress reporting every 30 seconds. No intermediate storage or temporary files on the database pods.

## Requirements

- Ansible 2.15+ with `kubernetes.core` collection and Python `kubernetes` client
- Kubernetes cluster access (in-cluster ServiceAccount or kubeconfig)
- For backup/restore: a writable path for `backups_path` (local, NFS, CephFS, etc.)

### Container Image

A ready-to-use Ansible image with all dependencies is available on Docker Hub:

```bash
docker run --rm \
  -v ~/.kube:/root/.kube:ro \
  -v /path/to/hasteward:/app:ro \
  docker.io/prplanit/ansible-oci:2.20.1-v2 \
  ansible-playbook /app/hasteward.yml \
    -e engine=cnpg \
    -e cluster_name=my-postgres \
    -e namespace=my-namespace \
    -e mode=triage
```

The image includes Ansible, `kubernetes.core` collection, and the Python `kubernetes` client.

## License

[AGPL-3.0-only](../LICENSE)
