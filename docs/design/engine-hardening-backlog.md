# Engine Hardening Backlog — CNPG & Galera triage/repair/bootstrap

Booked 2026-07-21. Gap inventory across both database engines, from a three-front
audit (Galera parity, helper-pod robustness, duplication + test coverage) plus the
authority-determination work in flight. Ordered by blast radius.

Guiding invariant for everything below: **a transient/connectivity/scheduling failure
must only ever produce an "unknown" (fail closed / refuse), never a fact** — it must
never discredit, exclude, or nuke a node that may hold the most-advanced data.

---

## P0 — Data-loss / wrong-authority bugs (fix + test first)

### P0.1 — [Galera] Transient `wsrep_recover` failure discredits the most-advanced node → wrong bootstrap target → data loss — ✅ FIXED IN SOURCE 2026-07-21 (`selectcandidate_test.go`; not yet shipped)
- **Where:** `src/engine/bootstrap/galera.go` — `executeBootstrap` STEP 4 (`:481-493`) + `selectCandidate` (`:785-834`).
- **Bug:** STEP 4 runs `RunWsrepRecover` per node; **any** failure (helper pod stuck ContainerCreating, image pull, 150s `ActiveDeadlineSeconds`, `Create` API error, "no output/timed out") just logs and `continue`s, excluding that node. `selectCandidate` then picks the max seqno **among only the nodes that recovered**, never comparing against the original candidate's Known triage seqno (`origGrastate` is fetched at `:816-820` but used only for logging). If the excluded node was the most-advanced, bootstrap force-declares a **less-advanced** node; the advanced node later SSTs from it, permanently discarding its committed transactions.
- **Why existing guards miss it:** the STEP 4.5 split-brain gate (`:509-513`) is `if bellyUp`-only; the all-fail case is safe (`:810-811`) and all-succeed is correct — only *partial* failure of the top node regresses.
- **Fix:** `selectCandidate` must refuse to override to a candidate whose recovered seqno is below the original candidate's **Known triage** seqno (`result.Decision.CandidateSeqno`); if a node with a Known/higher triage position fails `wsrep_recover`, **fail closed (abort bootstrap)** rather than proceed blind. Same rule the rest of the Galera engine already enforces.
- **Severity:** HIGH — the exact bug class that motivated the CNPG authority work, live in Galera bootstrap.

---

## P1 — Missing active-inspection capability + cluster-stays-down robustness

### P1.1 — [CNPG] No triage-time deep-recover (mirror Galera's `maybeDeepRecover`) — ✅ BUILT IN SOURCE 2026-07-21 (`cnpg_deeprecover.go` + `_test.go`; not yet shipped)
- **Where:** CNPG triage has rungs 0–1 (`runPVCProbes`) only; no analog of Galera `maybeDeepRecover`→`deepRecover` (`src/engine/triage/galera.go:1067/1108`).
- **Gap:** a stranded/crash-looping node holding its RWO PVC → `Unread` → `undeterminable` → repair refuses (safe, but the cluster stays wedged). Galera actively fences + `wsrep_recover`s to turn Unread→Known, then decides.
- **Build:** a CNPG `maybeDeepRecover` that **reuses `cnpgjob.OfflinePVCJob` read-only** (`OnPVCAcquired` execs `pg_controldata` + `.history`), mirroring Galera's discipline exactly:
  - **Paranoid double gate** (adopt Galera's): escalate only when provably nothing is alive — no live/Ready primary, and a fresh liveness probe finds nothing serving; if we cannot prove death (no creds / unreachable) **assume alive and stay hands-off**. Don't fence a live cluster to inspect one replica — that case is inspected in repair's already-fenced window, not triage.
  - **Restore on every exit path** (`OfflinePVCJob` already guarantees reconcile-restore + unfence).
  - **Triage-only:** reads positions into the authority inputs; never declares authority, sets a target, or writes.
- **Severity:** the second half of "do everything in our power to bring a node up for inspection before deciding."

### P1.2 — [Galera] Istio sidecar hang on the authority reads (cluster stays down in a mesh) — ✅ FIXED for the dangerous sites 2026-07-21 (not yet shipped)
Fixed via the shared builder (P2.4): `RunWsrepRecover` + `RunHelperPodSpec` (galera authority reads) now build through `k8s.BuildHelperPod`, which stamps the Istio exemption. CNPG probe + deep-recover migrated too. **Remaining un-migrated pod sites** (lower risk — mutating/escrow, not authority reads): `escrow/resticpvc.go`, `repair/cnpg.go` healPod, `repair/cnpg_breaker.go` clearPod, `prunewal/cnpg.go` walPod (these go through cnpgjob — its helper specs still hand-built), `reconfigure/galera.go` runHelperPod.

Original description:
- **Where:** `provider/galera_ops.go:259` (`RunWsrepRecover`) and `:403` (`RunHelperPodSpec`, which backs the galera grastate **triage probe** `triage/galera.go:442` *and* deep-recover). Both wait for `Succeeded`/`Failed` with **no `sidecar.istio.io/inject:"false"`**.
- **Bug:** an injected sidecar keeps the pod `Running` forever, so the `Succeeded`-wait never returns → 150s timeout → **bootstrap/deep-recover can't establish authority → cluster stays down/suspended.** This is the exact hazard the CNPG probe was hardened against (`triage/cnpg.go:503-504` is the only exempt site in the repo).
- **Fix:** add the exemption (label + annotation) and/or convert to Running+exec.

### P1.3 — [Both] No scheduling-failure diagnosis on any helper/probe/recover pod — ✅ PRIMITIVE + hot sites done 2026-07-21 (not yet shipped)
`k8s.DescribePodScheduling(pod)` + `k8s.PodUnschedulable(pod)` (pure, tested) turn a stuck pod into a reason (maxPods `Too many pods` / cordon / NotReady / image pull / pending). Wired into the timeout paths of `RunWsrepRecover`, `RunHelperPodSpec`, the CNPG `waitAndCollectProbe`, AND `cnpgjob`'s acquire loop — which now also **fails fast when the HELPER is Unschedulable** (instead of pointlessly churning the target pod). **Remaining:** wire into the other helper waits as they migrate.

Original description:
- **Where:** every pod-creation site (see table below). None inspects `pod.Status.Conditions` / `PodScheduled=false` / `Unschedulable`.
- **Bug:** a Pending pod from node `maxPods` 110/110 (a real incident), cordon, `NotReady`, or PVC `FailedAttach` is indistinguishable from a real failure — it silently burns the poll loop and reports a generic timeout. `cnpgjob`'s acquire loop even keeps deleting the **target** while the **helper** is the stuck one. `reconfigure/galera.go:560` is the only place with retry, and it matches error substrings, not the real scheduling reason.
- **Fix:** watch `PodScheduled=false`/`Unschedulable`, surface the reason, bounded retry.

---

## P2 — Transient-error hardening parity + shared abstractions + tests

### Transient-error hardening (Galera parity with the CNPG fixes)
- **P2.1 — [Galera] `QueryWsrep` transient failure → false "needs heal" → unnecessary SST.** ✅ FIXED 2026-07-21. Collect-time `QueryWsrep` now retries 3× (like the donor probe); on exhaustion it stores a sentinel with a new `wsrepStatus.Unread` flag. `buildAssessments` gets a dedicated `ws.Unread` case (ordered before the non-primary case) → "wsrep UNREAD, cannot classify — investigate before healing", `NeedsHeal=false`, so a blip no longer wipes+SSTs a healthy node.
- **P2.2 — [Galera] PVC `Get` conflates transient error with `NotFound` → false `ABORTING: Missing storage PVCs`.** ✅ FIXED 2026-07-21. New `pvcGetState()` returns `Bound` / `MISSING` (genuine NotFound) / `UNKNOWN` (transient, after 3 retries); the abort now distinguishes "could not determine PVC state (transient) — retry" from "genuinely missing". Same NotFound-vs-transient distinction as the CNPG fix.
- **P2.3 — [Both] Helper/probe timeouts hardcoded** (150s/120s) except the `cnpgjob` family; `reconfigure` even fetches `healTimeout` then ignores it. **Fix:** configurable + `ActiveDeadlineSeconds`.

### Shared abstractions (longevity — the `AuthorityOutcome`/`renderAuthorityBanner` model to copy)
- **P2.4 — Shared helper-pod builder.** ✅ BUILT 2026-07-21 as `k8s.BuildHelperPod(HelperPodOpts)` + `k8s.DisableIstioInjection` + `k8s.DescribePodScheduling` (`src/k8s/helperpod.go`, tested). Migrated: `galera_ops.go` RunWsrepRecover + RunHelperPodSpec, `triage/cnpg.go` probe, `triage/cnpg_deeprecover.go`. **Still to migrate** (build their pods through it): `repair/cnpg.go:316` healPod, `repair/cnpg_breaker.go:59` clearPod, `prunewal/cnpg.go:159` walPod, `escrow/resticpvc.go:86`, `reconfigure/galera.go:588`. Note: independent security-context fields (`RunAsUID`/`RunAsGID`/`FSGroup` each optional) preserve sites that deliberately omit FSGroup (a big-PVC recursive chown).
- **P2.5 — Shared "safe fenced operation" contract.** CNPG `cnpgjob.OfflinePVCJob` (Lease-serialized) and Galera's hand-rolled `deepRecover`/`healNode` (annotation-serialized) are two parallel contracts; the restore-on-all-paths discipline is implemented **3×** (`cnpgjob.restoreReconciliation`, galera `deepRecover` defers, galera `healNode.rescue()`), and mutual exclusion uses two mechanisms (Lease vs CR annotation) for the same goal. Unify into one `FencedOperation` (acquire-lock → quiesce operator → wait pods-gone/PVC-free → do work → guaranteed restore) with engine hooks; pick ONE exclusion mechanism.
- **P2.6 — Shared authority-verdict assembly.** ✅ DONE 2026-07-23 (`triage/authority.go` + `authority_test.go`): `newAuthorityComparison(outcome, mostAdvanced, value, reasons, okMsg)` (the shared DataComparison constructor — SafeToHeal/Authority/Warnings/SplitBrainDetails), `authorityWarnings(safe, okMsg, reasons)`, and `deriveAuthorityStatus(safeToHeal)`. Both engines' `crossInstanceComparison` + `Analyze` now build through them; engine-specific fields (CNPG CheckpointLocation, Galera PrimaryMembers/BestPrimarySeqno; the donor pick) stay per-engine. **Deliberately NOT done:** unifying the per-node legibility TYPE — CNPG's 3-valued `ReadState` and Galera's 2-valued `Known`+unread reflect genuinely different authority models, and forcing one type onto both would rewrite well-tested subtle ranking code for little gain (over-abstraction). The actual duplication was the OUTPUT assembly, which is now shared.
- **P2.7 — [CNPG adopt from Galera] explicit `Known` provenance predicate.** Galera cleanly separates authoritative reads from hints (`effectiveSeqno.Known`, `isAuthoritativeRecover`) so hints are structurally barred from establishing authority. CNPG should carry an equivalent explicit provenance on its read positions.
- **P2.8 — Small shared helpers.** `provider.Ordinal(pod)` (inlined 6×: `cnpg.go:1386`, `galera.go:724`, `cnpg.go:854`, `repair/cnpg.go:273`, `repair/cnpg_breaker.go:36`, prunewal); one probe-name join (`joinCNPGProbeNames`/`joinGaleraProbeNames` are identical); Galera adopt CNPG's `DiskStats` collector (has only `parseDiskPercent`); collapse CNPG's two section parsers (`extractSection` + inline in `parseDiskStats`); shared PVC-state helper carrying the `NotFound`-vs-transient distinction (fixes P2.2).

### Test gaps (safety-critical, currently no/thin coverage — ranked by blast radius)
- **P2.T1 — `resolveAutoDonor`/`resolveExplicitDonor`** — ✅ DONE 2026-07-21 (`galera_donor_test.go`): ambiguous refuses auto-select even with Synced candidates + `--force`; unambiguous-no-candidates aborts; explicit out-of-range ordinal aborts. (The k8s-exec paths of `probeWsrep`/explicit-suitability remain flow-test-only — the fake clientset can't exec.)
- **P2.T2 — `galera buildAssessments`** — ✅ DONE 2026-07-21 (`galera_assessments_test.go`): Unread → `!NeedsHeal`; disconnected → `NeedsHeal`; Synced-Primary → `!NeedsHeal`; ahead-of-primary under `!SafeToHeal` → manual `!NeedsHeal`.
- **P2.T3 — `cnpg buildAssessments`** — ✅ DONE 2026-07-21 (`cnpg_assessments_test.go`): primary never heals; behind-timeline → heal; same-TL streaming → no heal; same-TL stranded (behind+not-streaming+crashloop) → heal.
- **P2.T4 — `cnpg PreAssess` deadlock-breaker flow** — ✅ PARTIAL 2026-07-23 (`cnpg_preassess_test.go`, via a fake Triager): inert without `--unwedge`; defers when no breakable deadlock; **REFUSES a disk-full deadlock when authority is ambiguous** (the key `rm -rf pgdata` guard). REMAINING (needs a k8s+escrow harness): the post-gate orchestration — escrow-space refusal, proof-hash drift re-check, escrow-retained-on-clear-failure.
- **P2.T5 — `ParseWsrepRecoverOutput`** — ✅ DONE 2026-07-23 (`provider/galera_recover_parse_test.go`): rejects ZeroUUID / negative / phantom seqno; LastCommitted falls back to seqno; Valid only on clean parse.
- **P2.T6 — `scrapeRecoveredPosition`** — ✅ DONE 2026-07-23. Extracted the pure `parseRecoveredPositionFromLog`; test (`galera_parse_test.go`) covers both log formats, max-across-lines, and `(-1,"")` when absent.
- **P2.T7 — `cnpg buildAuthorityInputs` ReadState branches** — ✅ DONE 2026-07-23 (`cnpg_authorityinputs_test.go`): all 8 branches asserted directly, incl. transient `UNKNOWN` → Unread (not AbsentNoData) vs `MISSING` → AbsentNoData.
- **P2.T8 — `galera crossInstanceComparison` divergence/precedence** — ✅ DONE 2026-07-23 (`galera_comparison_test.go`): Known seqno-ahead-of-primary → Diverged; unread dominates divergence → Undeterminable.
- **P2.T9 — `maybeDeepRecover`/`anyServing` fence gate** — ✅ DONE 2026-07-23. `isAnythingAlive` covered (`TestIsAnythingAlive` + the CNPG analog `TestCnpgIsAnythingAlive`); `anyServing` empty-password fail-safe (`galera_parse_test.go`). REMAINING: the exec-positive `SELECT 1` branch (needs fake exec).

---

## Pod-creation site robustness matrix (from the helper-pod audit)

| Site | Purpose | Istio | Phase wait | Sched diag | Cleanup | Timeout |
|---|---|---|---|---|---|---|
| `galera_ops.go:259` RunWsrepRecover | fenced seqno read | ✗ | Succeeded (hangs w/ sidecar) | ✗ | ok | 150s hard |
| `galera_ops.go:403` RunHelperPodSpec | grastate probe / heal / clear | ✗ | Succeeded (hangs) | ✗ | ok | 150s hard |
| `triage/cnpg.go:494` runPVCProbes | pg_controldata/history probe | ✓ (only site) | Succeeded (ok ∵ exempt) | ✗ | no defer (panic leak) | 150s hard |
| `escrow/resticpvc.go:86` captureOne | tar PVC → restic | ✗ | Running (robust) | ✗ | defer (best) | 120s hard |
| `repair/cnpg_breaker.go:59` clearPod | clear pgdata | ✗ | Succeeded | ✗ | best (cnpgjob) | configurable |
| `repair/cnpg.go:316` healPod | clear + basebackup | ✗ | Succeeded | ✗ | best (cnpgjob) | configurable |
| `prunewal/cnpg.go:159` walPod | WAL prune (Go-exec) | ✗ | Running (robust) | ✗ | best (cnpgjob) | configurable |
| `cnpgjob/offlinejob.go:118` Run | shared fence/handoff | (caller) | Running+Succeeded | ✗ (deletes target, not helper) | excellent (detached-ctx restore) | configurable |
| `reconfigure/galera.go:588` runHelperPod | grastate script | ✗ | Succeeded | partial (err-string match) | ok | 150s hard |

`restore/{cnpg,galera}.go` create no pods (delete + let the operator re-clone).

---

## P3 — design gaps exposed by the boundary-postgres recovery (2026-07-26)

A real CNPG split-brain (stale-restore primary on TL9 vs golden crash-looping replica on TL8, forked at `2C/99`). Detection worked (refused correctly); the gaps are in *resolving* and *recovering*.

- **P3.1 — triage detects divergence but doesn't help RESOLVE it.** ✅ DONE. It used to say "diverged, choose manually" and stop; the evidence that actually made the call (WAL volume past the fork per branch — 423 GB vs 9 GB — and "TL9's writes are 100% job-scheduler + heartbeat churn, no sessions/config") was gathered by hand. Now surfaced automatically on divergence / leader-not-primary. **Part A: per-branch WAL volume past the fork in the divergence verdict (`describeDivergence`/`formatWALPastFork`), with a "weigh content not size" caveat.** **Part B: the running primary's per-table write ledger (top pg_stat_user_tables by ins+upd+del) rides along on the verdict (`collectWriteActivity`/`formatWriteLedger`) so the operator sees churn vs real.** Best-effort, READ-ONLY, primary-only, and it SURFACES — never classifies (churn-vs-real is app-specific; the operator judges which tables are "real").
- **P3.2 — recovery assumes primary = authority.** repair/heal/breaker all clone replicas FROM the primary; `--unwedge` clears disposables and keeps the primary. When the authority is a crash-looping REPLICA and the primary is the stale lineage (this case), there is NO clean "promote the replica, rebuild the primary from it" flow. The `leader_not_primary` outcome is DETECTED but the repair engine can't act on it. Highest-value recovery gap.
- **P3.3 — no restore-regression guard.** A restore/heal that rewinds BEHIND live data (what created `-3`/TL9 from an old `2C/99` point) should refuse/warn — the authority principle applied to restore TARGETS, not just heals.
- **P3.4 — a stuck authority isn't auto-relieved.** The golden `-2` crash-looped on disk-full WAL for days (data at risk) while `prunewal` (the fix) is manual and oriented at non-authority nodes. Triage/repair should prioritize WAL relief for a data-bearing / candidate-authority node stuck on disk-full.
- **P3.5 — no pathological-history / restore-loop health signal.** boundary-postgres has 9 timelines with a non-monotonic rewind — a screaming sign of an unhealthy recovery process. Flag it proactively (N timelines + a detected rewind) instead of silently coping.

Causality note: no proof hasteward caused the TL9 rewind (cluster bootstraps `initdb`, no backup configured → CNPG can't auto-restore; the rewind was manual/tool-driven; hasteward's old highest-timeline bug *could* have misguided a prior recovery but unconfirmed). The current shred-pressure on `-2` is CNPG's normal reconcile, not hasteward.
