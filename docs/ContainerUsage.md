# Container Usage

How to drive HASteward as a **one-shot container** against a Kubernetes cluster — the form you use
when the binary isn't on `$PATH` (e.g. operating a remote cluster from a workstation). For the bare
`hasteward <cmd>` form (binary on PATH / in-cluster), see [Examples](Examples.md).

Every command below is a **complete, copy-pasteable `docker run`** — nothing is hidden behind an
alias. Replace the `<PLACEHOLDERS>`. (If you run these a lot, there's an optional shortcut at the
[bottom](#optional-shortcut).)

> Image: `docker.io/prplanit/hasteward:latest` (`latest` = release, `latest-dev` = dev).

## Prerequisites

- `docker` on your machine, and a working `kubectl` (a kubeconfig at `~/.kube/config` that reaches the
  cluster). The container reuses that kubeconfig.
- `--network host` makes the container reach the kube API exactly as your `kubectl` does.

| Placeholder | Meaning | Example |
|---|---|---|
| `<ENGINE>` | database engine | `cnpg` (PostgreSQL/CloudNativePG) or `galera` (MariaDB) |
| `<CLUSTER>` | cluster CR name | `zitadel-postgres` |
| `<NAMESPACE>` | its namespace | `zeldas-lullaby` |
| `<N>` | instance number to repair | `3` |

There are **two shapes**: read-only commands need only your kubeconfig; commands that take a backup
(`repair`, `backup`, `restore`, `export`, `prune backups`, `get backups`) also need a restic repo +
password + a writable temp.

---

## Read-only commands (kubeconfig only)

### Triage — diagnose a cluster (ALWAYS run this first)

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  docker.io/prplanit/hasteward:latest \
  triage -e <ENGINE> -c <CLUSTER> -n <NAMESPACE>
```

Concrete:

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  docker.io/prplanit/hasteward:latest \
  triage -e cnpg -c zitadel-postgres -n zeldas-lullaby --output json
```

### Status of all managed clusters

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  docker.io/prplanit/hasteward:latest \
  get status -e <ENGINE> -n <NAMESPACE>
```

### Emergency WAL prune (CNPG disk-full deadlock — no backup needed)

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  docker.io/prplanit/hasteward:latest \
  prune wal -e cnpg -c <CLUSTER> -n <NAMESPACE>
```

---

## Backup-bearing commands (kubeconfig + restic repo)

These add four things to every `docker run`:

```text
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD"          # repo encryption password
  -v "$HOME/hasteward-escrow:/backups"           # host dir holding the restic repo
  --tmpfs /tmp:size=4g                           # writable, ephemeral temp for restic packs
  -e RESTIC_CACHE_DIR=/backups/.restic-cache      # persistent restic index cache (on the host dir)
  -e TMPDIR=/tmp
```

First-time setup of the repo + password (once per machine):

```bash
mkdir -p "$HOME/hasteward-escrow"
openssl rand -hex 16 > "$HOME/hasteward-escrow/.restic-password"
chmod 600 "$HOME/hasteward-escrow/.restic-password"
export RESTIC_PASSWORD=$(cat "$HOME/hasteward-escrow/.restic-password")
```

### Repair ONE diverged/unhealthy instance (heal = re-clone from the primary)

Preview first with `--dry-run`, then drop it for the real run:

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
  -v "$HOME/hasteward-escrow:/backups" \
  --tmpfs /tmp:size=4g \
  -e RESTIC_CACHE_DIR=/backups/.restic-cache \
  -e TMPDIR=/tmp \
  docker.io/prplanit/hasteward:latest \
  repair -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> \
    --instance <N> --backups-path /backups --output jsonl --verbose --dry-run
```

Add `--donor <N>` to force the authoritative source, `--force` for ambiguous Galera split-brain, or
`--no-escrow` to skip the pre-repair backup (only if you already have a recent one).

### Repair ALL unhealthy instances at once

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
  -v "$HOME/hasteward-escrow:/backups" \
  --tmpfs /tmp:size=4g \
  -e RESTIC_CACHE_DIR=/backups/.restic-cache \
  -e TMPDIR=/tmp \
  docker.io/prplanit/hasteward:latest \
  repair -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> --backups-path /backups --output jsonl
```

### Backup / list / restore / export / prune

These are the **same `docker run` block** as the repair example above — only the **final
`hasteward …` line changes**. Here is the full command (listing snapshots):

```bash
docker run --rm --network host \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" \
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
  -v "$HOME/hasteward-escrow:/backups" \
  --tmpfs /tmp:size=4g \
  -e RESTIC_CACHE_DIR=/backups/.restic-cache \
  -e TMPDIR=/tmp \
  docker.io/prplanit/hasteward:latest \
  get backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups
```

For the others, keep everything above identical and replace **only that last line** with one of:

```text
backup        -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> --backups-path /backups        # take a backup (-m native for engine-native)
get backups   -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> --backups-path /backups        # list snapshots
restore       -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> --backups-path /backups --snapshot latest
export        -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> --backups-path /backups --snapshot <ID>   # → .sql.gz in your host dir
prune backups -e <ENGINE> -c <CLUSTER> -n <NAMESPACE> --backups-path /backups        # apply retention
```

---

## Worked example — healing `zitadel-postgres` end to end

**Symptom:** cluster phase `Not enough disk space`; `zitadel-postgres-1` CrashLoopBackOff, `-3` not
ready (only the primary `-2` ready). The replicas diverged onto an older timeline after a failover and
can't catch up via streaming — they need a `pg_basebackup` re-clone.

```bash
# 1. Triage (read-only) — confirm the picture.
docker run --rm --network host \
  -e KUBECONFIG=/kube/config -v "$HOME/.kube:/kube:ro" \
  docker.io/prplanit/hasteward:latest \
  triage -e cnpg -c zitadel-postgres -n zeldas-lullaby --output json
#  → primary -2 healthy (timeline 49); -1 & -3 needHeal (timeline 48); safeToHeal: true

# 2. One-time: create the escrow repo + password (this cluster had no backups).
mkdir -p "$HOME/hasteward-escrow"
openssl rand -hex 16 > "$HOME/hasteward-escrow/.restic-password"
chmod 600 "$HOME/hasteward-escrow/.restic-password"
export RESTIC_PASSWORD=$(cat "$HOME/hasteward-escrow/.restic-password")

# 3. Heal a diverged replica (repeat with --instance 1, then 3, etc.).
#    Add --dry-run first to preview, then run without it.
docker run --rm --network host \
  -e KUBECONFIG=/kube/config -v "$HOME/.kube:/kube:ro" \
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
  -v "$HOME/hasteward-escrow:/backups" \
  --tmpfs /tmp:size=4g -e RESTIC_CACHE_DIR=/backups/.restic-cache -e TMPDIR=/tmp \
  docker.io/prplanit/hasteward:latest \
  repair -e cnpg -c zitadel-postgres -n zeldas-lullaby \
    --instance 3 --backups-path /backups --output jsonl --verbose

# 4. Verify.
kubectl get cluster zitadel-postgres -n zeldas-lullaby   # → READY 3/3, "Cluster in healthy state"
```

The repair phases stream as jsonl: `assess → safety-gate → escrow → plan → heal`. The escrow
(`pg_dumpall | restic backup --stdin`) runs **before** any destructive step, and the primary is only
the `pg_basebackup` source — it is never modified — so the application stays up throughout.

> Tip: re-run **triage** between heals. An instance can recover on its own (e.g. once disk frees up),
> so you may find you only need to heal the ones still on the old timeline.

---

## Running as an in-cluster Kubernetes Job

For **scheduled** or **production** use, run HASteward *inside* the cluster as a Job rather than from a
workstation. It's cleaner: the Job mounts an `emptyDir` at `/tmp` (restic's temp packs just work — no
`--tmpfs` needed), a PVC at `/backups` (a durable in-cluster restic repo), and uses a ServiceAccount
with least-privilege RBAC — no kubeconfig to mount, no host networking.

The manifest is committed at [`docs/k8s/job.yaml`](k8s/job.yaml) — ServiceAccount + ClusterRole(Binding)
(cluster-wide, so it can operate on DB clusters in any namespace) + a `hasteward-backups` PVC + the
`hasteward-run` Job.

```bash
# 1. One-time: RBAC + ServiceAccount + the backups PVC.
kubectl apply -f docs/k8s/job.yaml -l app.kubernetes.io/component=rbac
kubectl apply -f docs/k8s/job.yaml -l app.kubernetes.io/component=storage

# 2. One-time: the restic password secret the Job reads.
kubectl create secret generic hasteward-restic \
  --from-literal=password="$(openssl rand -hex 16)" -n <JOB_NAMESPACE>

# 3. Per run: edit the Job's `args:` + env in job.yaml, then (re)apply and watch.
#    args: ["triage"]  →  ["repair","-i","3","--output","jsonl"]
#    env:  HASTEWARD_ENGINE / HASTEWARD_CLUSTER / HASTEWARD_NAMESPACE
kubectl delete job hasteward-run -n <JOB_NAMESPACE> --ignore-not-found
kubectl apply -f docs/k8s/job.yaml -l app.kubernetes.io/component=job
kubectl logs -f job/hasteward-run -n <JOB_NAMESPACE>
```

The Job passes the same knobs as the CLI, via env: `HASTEWARD_ENGINE` (`-e`), `HASTEWARD_CLUSTER`
(`-c`), `HASTEWARD_NAMESPACE` (`-n`), `HASTEWARD_BACKUPS_PATH` (`--backups-path`), and
`RESTIC_PASSWORD` (from the `hasteward-restic` secret); `args:` is the subcommand + flags. So
`args: ["repair","-i","3"]` is the in-cluster equivalent of `docker run … repair -e cnpg -c … -n … -i 3`.

> Prefer this for scheduled backups/triage and production repairs: the escrow tempdir/cache "just
> works" (emptyDir `/tmp`, PVC `/backups`), and the RBAC is least-privilege per `docs/security.md`.
> For one-off interactive ops from your laptop, the `docker run` form above is quicker.

## Notes

- **Order of operations:** `triage` → `--dry-run` → real run. `triage` / `get *` only read; `repair`,
  `backup`, `restore`, `prune *`, `bootstrap` mutate.
- **Output formats:** `--output jsonl` streams per-phase events — prefer it for long ops; the default
  `human` formatter buffers and can *look* hung while probing an unreachable instance. `--output json`
  is a single machine-readable envelope.
- **Why `--tmpfs /tmp` + `RESTIC_CACHE_DIR` are load-bearing (and not optional):** the escrow streams
  `pg_dumpall | restic backup --stdin`. restic writes **transient pack files** to `$TMPDIR`, so it
  needs a writable `/tmp` — the tmpfs gives it a fast, ephemeral, in-RAM one. It also keeps a
  **persistent index cache**; pointing `RESTIC_CACHE_DIR` at `/backups/.restic-cache` (the host dir)
  makes that cache survive runs so repeat ops don't re-fetch the index. Without a writable `/tmp` the
  escrow dump pipe **deadlocks** instead of erroring — a HASteward bug to fix (provision its own
  tempdir + fail fast on restic error).
- **Timeouts:** `--heal-timeout` (default 600s) and `--delete-timeout` (default 300s) bound the wait
  on the re-clone and pod/PVC deletion.
- **No host binary needed:** everything runs from the published image; HASteward itself is built
  in-container by StageFreight (no local Go toolchain).

---

## Optional shortcut

Tired of the long line? Drop this in `~/.bashrc` and then run `hw <subcommand> …`:

```bash
hw() {
  docker run --rm --network host \
    -e KUBECONFIG=/kube/config -v "$HOME/.kube:/kube:ro" \
    -e RESTIC_PASSWORD="${RESTIC_PASSWORD:-}" \
    -v "${HW_ESCROW:-$HOME/hasteward-escrow}:/backups" \
    --tmpfs /tmp:size=4g -e RESTIC_CACHE_DIR=/backups/.restic-cache -e TMPDIR=/tmp \
    docker.io/prplanit/hasteward:latest "$@"
}
# e.g.  hw triage -e cnpg -c zitadel-postgres -n zeldas-lullaby
#       hw repair -e cnpg -c zitadel-postgres -n zeldas-lullaby -i 3 --backups-path /backups --output jsonl
```

(The `RESTIC_*`/`/backups`/`--tmpfs` bits are harmless on read-only commands; they're just unused.)
