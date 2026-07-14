# Operator Mode

```bash
hasteward serve
```

The operator watches CNPG Cluster and MariaDB CRs for `clinic.hasteward.prplanit.com/policy` annotations and runs scheduled backups and triage/repair operations.

## CRDs

**BackupPolicy** (cluster-scoped) — defines global defaults:

```yaml
apiVersion: clinic.hasteward.prplanit.com/v1alpha1
kind: BackupPolicy
metadata:
  name: default
spec:
  backupSchedule: "0 2 * * *"
  triageSchedule: "*/15 * * * *"
  mode: repair
  repositories:
    - local-backups
  retention:
    keepLast: 7
    keepDaily: 30
    keepWeekly: 12
    keepMonthly: 24
```

**BackupRepository** (cluster-scoped) — defines restic repo connection:

```yaml
apiVersion: clinic.hasteward.prplanit.com/v1alpha1
kind: BackupRepository
metadata:
  name: local-backups
spec:
  restic:
    repository: /backups/restic/local
    passwordSecretRef:
      name: restic-password
      namespace: fairy-bottle
      key: password
    envSecretRef:              # optional, for S3
      name: s3-credentials
      namespace: fairy-bottle
```

## Database CR Opt-In

Add annotations to CNPG Cluster or MariaDB CRs:

```yaml
metadata:
  annotations:
    clinic.hasteward.prplanit.com/policy: "default"
    # Optional overrides:
    clinic.hasteward.prplanit.com/backup-schedule: "0 3 * * *"
    clinic.hasteward.prplanit.com/mode: "triage"
    clinic.hasteward.prplanit.com/exclude: "true"
```

## Operator Endpoints

| Endpoint | Description |
|----------|-------------|
| `:8080/metrics` | Prometheus metrics |
| `:8081/healthz` | Liveness probe |
| `:8081/readyz` | Readiness probe |
