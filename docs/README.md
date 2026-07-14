---
title: HASteward
template: home.html
hide:
  - navigation
  - toc
---

<div class="grid cards hw-features" markdown>

-   **Triage, read-only**

    ---

    Diagnose a cluster's health — replicas, quorum, primary, lag — without touching a
    thing. Every repair starts here.

    [Examples →](usage/examples.md)

-   **Repair, safely**

    ---

    Heal unhealthy instances with a pre-repair escrow backup and refusal gates for
    ambiguous states. `--dry-run` shows the plan first.

    [Safety Gates →](usage/safety-gates.md)

-   **Backup & Restore**

    ---

    restic-backed snapshots — block-level dedup, encryption, compression — with retention
    and point-in-time restore.

    [Backups →](usage/backups.md)

-   **Operator Mode**

    ---

    Run it in-cluster: CRDs, annotations, and a scheduler that triages and backs up on a
    cadence.

    [Operator →](usage/operator.md)

-   **CNPG + Galera**

    ---

    One steward for CloudNativePG (PostgreSQL) and MariaDB Operator (Galera), with
    engine-aware repair flows.

    [Architecture →](design/index.md)

-   **Container-Native**

    ---

    A single scratch image with the binary + `restic`. Drop it into a Job, a sidecar, or
    your shell.

    [Container Usage →](usage/container-usage.md)

</div>

<div class="hw-hero-foot" markdown>

**Keep the cluster up. Let the steward handle the rest.** →
[Browse the docs](design/index.md)

</div>
