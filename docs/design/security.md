# Security Threat Model

Analysis of HASteward's backup security posture, known gaps, and hardening strategy.

## Current Protections

| Threat | Protected? | How |
|--------|-----------|-----|
| Accidental data deletion (DROP TABLE, bad migration) | Yes | Restic snapshots with retention policy, point-in-time restore |
| Pod/node failure | Yes (operators) | CNPG/Galera replication handles this, HASteward repairs stragglers |
| Split-brain / diverged replicas | Yes | Diverged backups capture each instance's state before repair |
| Operator bug corrupts cluster state | Yes | Pre-repair escrow backup before any destructive action |
| Human error during repair | Partial | Escrow + diverged snapshots preserve pre-repair state |

## Unprotected Threats

### 1. Restic repository is mutable (CRITICAL)

Anyone who can write to the backup path can destroy all history:

```
restic forget --keep-last 0 --unsafe-allow-remove-all
restic prune
```

On CephFS there is no object lock, no versioning, no recycle bin. The bits are zeroed.

A compromised hasteward pod (or any workload with the ServiceAccount token + restic password) has full read/write/delete access to the entire backup repository.

### 2. Single encryption key (HIGH)

Every backup across all clusters, engines, and namespaces uses one `RESTIC_PASSWORD`. Compromise of that one secret decrypts all backups for all clusters. The password exists in:
- Kubernetes Secrets
- Environment variables on jobs
- Vault (if configured)

### 3. Same blast radius for live data and backups (HIGH)

If backups land on the same storage cluster as live data (e.g., CephFS for backups, Ceph RBD for databases), a storage-level failure kills both simultaneously. A bad OSD map, pool corruption, or cluster-wide outage takes out live data and backups together.

### 4. Overprivileged ServiceAccount (MEDIUM)

The hasteward ServiceAccount currently uses `cluster-admin`. This gives it access to every resource in the cluster, far exceeding what it needs. A compromised hasteward pod could:
- Delete arbitrary PVCs, Secrets, and workloads
- Mount and destroy the backup PVC
- Escalate to any namespace

HASteward only needs: pod exec/get/list/create/delete, secret read, PVC read, StatefulSet scale, CNPG/MariaDB CR patch, and its own CRDs. See `deploy/rbac/clusterrole.yaml` for the scoped role.

### 5. No offsite copy (MEDIUM)

Everything is in one physical location. No protection against site loss (fire, flood, power, theft).

### 6. No scheduled integrity checks (LOW)

We trust the restic repository is healthy but never verify. `restic check` detects data corruption, missing blobs, and index inconsistencies. A corrupted backup discovered at restore time means no backup.

### 7. No automated restore verification (LOW)

Backups are never test-restored. A backup that completes successfully may produce an unusable dump (e.g., partial data, encoding issues).

## Hardening Strategy

### Tier 1: Least-Privilege RBAC

Replace `cluster-admin` with the scoped ClusterRole in `deploy/rbac/clusterrole.yaml`. The hasteward binary needs:
- `pods`, `pods/exec`, `pods/log` — triage, dump/restore streaming, heal pod logs
- `secrets` (get) — read database credentials, TLS certs, repo passwords
- `persistentvolumeclaims` (get/list) — triage disk checks
- `statefulsets/scale` (get/update) — Galera node healing
- `clusters` (postgresql.cnpg.io) — get/list/patch for fencing
- `backups` (postgresql.cnpg.io) — native backup method
- `mariadbs` (k8s.mariadb.com) — get/list/patch for suspend/resume
- `backuprepositories`, `backuppolicies` (hasteward CRDs) — operator mode
- `events` — emit Kubernetes events
- `leases` — leader election (operator mode)

This eliminates the ability to delete arbitrary cluster resources. The ServiceAccount can still exec into database pods (required for dumps) and read secrets (required for credentials), but cannot destroy PVCs, workloads, or backup storage through the Kubernetes API.

### Tier 2: S3 with Object Lock (Immutable Backups)

Move the primary backup target from CephFS to S3 (Ceph RGW or MinIO) with object lock enabled.

**How it works:**
- S3 object lock (Compliance mode) prevents deletion/overwrite of objects until the retention period expires
- Even with valid S3 credentials, `restic forget` + `restic prune` cannot delete the underlying data blobs — S3 returns 403
- An attacker can mark snapshots as forgotten in restic metadata, but the actual data is physically immutable
- A clean `restic rebuild-index` from a second machine recovers everything

**Write-only IAM policy:**
- The hasteward S3 user gets `PutObject` but not `DeleteObject`
- Restic backup works (only PUTs)
- Restic forget/prune fails (cannot delete)
- A separate admin S3 user (stored outside the cluster, break-glass) performs legitimate pruning
- Pruning becomes an explicit out-of-band operation, never automated from within the cluster

**Governance vs Compliance mode:**
- Governance: admin can override the lock (useful for testing)
- Compliance: nobody can override, not even the bucket owner, until retention expires

For backups, Compliance mode is the point — protecting against your own infrastructure being compromised.

**Bucket setup (S3 API):**

```bash
# Create bucket with object lock enabled (must be set at creation time)
aws s3api create-bucket \
  --bucket hasteward-immutable \
  --object-lock-enabled-for-object-lock-configuration

# Set default retention (Compliance mode, 30 days)
aws s3api put-object-lock-configuration \
  --bucket hasteward-immutable \
  --object-lock-configuration '{
    "ObjectLockEnabled": "Enabled",
    "Rule": {
      "DefaultRetention": {
        "Mode": "COMPLIANCE",
        "Days": 30
      }
    }
  }'
```

**Write-only IAM policy for the hasteward S3 user:**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:GetObject",
        "s3:ListBucket",
        "s3:GetBucketLocation"
      ],
      "Resource": [
        "arn:aws:s3:::hasteward-immutable",
        "arn:aws:s3:::hasteward-immutable/*"
      ]
    },
    {
      "Effect": "Deny",
      "Action": [
        "s3:DeleteObject",
        "s3:DeleteObjectVersion",
        "s3:PutObjectLockConfiguration",
        "s3:PutBucketObjectLockConfiguration"
      ],
      "Resource": "*"
    }
  ]
}
```

Restic needs `PutObject` (backup), `GetObject` (restore/check), and `ListBucket` (snapshots). The explicit `Deny` on delete operations ensures even if additional policies are attached, deletion is blocked. Object lock provides a second layer — even with `DeleteObject` permission, compliance-mode objects cannot be deleted until retention expires.

### Tier 3: Per-Cluster Encryption Keys

Use separate `RESTIC_PASSWORD` values per BackupRepository. Compromise of one key exposes only that cluster's backups. The `BackupRepository` CRD already supports per-repo passwords via `passwordSecretRef`.

### Tier 4: Tiered Backup Architecture

```
Tier 1 (fast, mutable):      CephFS in-cluster — working restic repo
Tier 2 (local, immutable):   S3 with object lock — ransomware-resistant
Tier 3 (offsite, immutable): Cloud S3 with object lock — disaster recovery
```

- Tier 1 protects against accidental deletion (quick restore, fast backup)
- Tier 2 protects against cluster compromise (separate blast radius, immutable)
- Tier 3 protects against site loss (different physical location)

The `BackupPolicy` CRD supports multiple repositories. The operator can backup to all tiers on each run, or `restic copy` from Tier 1 to Tier 2/3 on a schedule.

### Tier 5: Scheduled Integrity Checks

Run `restic check` on a schedule (weekly or after each backup). The operator can include this as a post-backup step. Alerts on failure via Prometheus metrics (`hasteward_repository_check_result`).

## Filesystem Immutability Limitations

S3 object lock is the correct solution. POSIX filesystems cannot provide equivalent guarantees:

| Approach | Why it fails |
|----------|-------------|
| `chattr +i` (Linux immutable) | CephFS doesn't support it. Requires root on the storage node. |
| CephFS snapshots | Admin-managed, outside Kubernetes. Safety net, not real immutability. |
| Read-only mount | HASteward needs write access to create backups. |
| NFS root_squash | Fragile, non-standard, Ceph doesn't provide it. |

The fundamental problem: POSIX filesystems don't have object lock semantics. If a process can create a file, it can generally delete it. Only object storage provides the "write but never delete" access model.

CephFS can remain as a fast mutable cache (Tier 1) for quick backup/restore operations, but the immutable copy must live on S3.

## On-Premises S3 Considerations

For deployments where an S3-compatible gateway (MinIO, Ceph RGW, etc.) runs on-prem:

| Threat | Same-site S3 with object lock? |
|--------|-------------------------------|
| Compromised hasteward pod / ServiceAccount | **Yes** — pod only has S3 write credentials |
| Compromised S3 credentials (leaked IAM key) | **Yes** — object lock denies DeleteObject |
| Compromised cluster (full cluster-admin) | **Depends** — only if S3 gateway is outside the cluster |
| Compromised hypervisor / root on S3 host | **No** — root can `rm` the files directly |
| Ransomware across the whole network | **No** — same site, same network |
| Site loss | **No** — same building |

The S3-compatible gateway should run **outside the Kubernetes cluster** (e.g., Docker on a separate ZFS host). Additional hardening:
- ZFS snapshots on the S3 data directory (automated, retained N days)
- `zfs hold` to prevent snapshot destruction without explicit release
- Offsite copy (cloud S3) for site-loss protection

## Restic Copy for Cross-Repo Transfer

`restic copy` transfers snapshots between repositories with different backends and different passwords, deduplicating during the copy:

```
restic -r /backups copy \
  --repo2 s3:http://minio:9000/hasteward-immutable \
  --password-file2 s3.key
```

This enables the tiered architecture: fast local backup, then async copy to immutable S3. The two repos have different passwords so compromising one doesn't expose the other.
