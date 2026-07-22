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
- **P2.6 — Shared read-state / legibility abstraction.** CNPG `ReadState` (3-valued Unread/Read/AbsentNoData, `cnpg_authority.go:31`) generalizes Galera's `effectiveSeqno.Known` bool + inline `unread` slice. One shared per-node legibility type + one "any Unread ⇒ Undeterminable" gate feeding `model.AuthorityOutcome`. Also: the outcome→`DataComparison` assembly switch is duplicated in spirit (`cnpg.go:749` vs `galera.go:639`) → one shared `DataComparison` constructor `(outcome, leader, primary, reasons)`; and `AuthorityStatus`+`RecommendedDonor` derivation (`cnpg.go:655` vs `galera.go:492`) → one shared projection.
- **P2.7 — [CNPG adopt from Galera] explicit `Known` provenance predicate.** Galera cleanly separates authoritative reads from hints (`effectiveSeqno.Known`, `isAuthoritativeRecover`) so hints are structurally barred from establishing authority. CNPG should carry an equivalent explicit provenance on its read positions.
- **P2.8 — Small shared helpers.** `provider.Ordinal(pod)` (inlined 6×: `cnpg.go:1386`, `galera.go:724`, `cnpg.go:854`, `repair/cnpg.go:273`, `repair/cnpg_breaker.go:36`, prunewal); one probe-name join (`joinCNPGProbeNames`/`joinGaleraProbeNames` are identical); Galera adopt CNPG's `DiskStats` collector (has only `parseDiskPercent`); collapse CNPG's two section parsers (`extractSection` + inline in `parseDiskStats`); shared PVC-state helper carrying the `NotFound`-vs-transient distinction (fixes P2.2).

### Test gaps (safety-critical, currently no/thin coverage — ranked by blast radius)
- **P2.T1 — `resolveAutoDonor`/`resolveExplicitDonor`** — ✅ DONE 2026-07-21 (`galera_donor_test.go`): ambiguous refuses auto-select even with Synced candidates + `--force`; unambiguous-no-candidates aborts; explicit out-of-range ordinal aborts. (The k8s-exec paths of `probeWsrep`/explicit-suitability remain flow-test-only — the fake clientset can't exec.)
- **P2.T2 — `galera buildAssessments`** — ✅ DONE 2026-07-21 (`galera_assessments_test.go`): Unread → `!NeedsHeal`; disconnected → `NeedsHeal`; Synced-Primary → `!NeedsHeal`; ahead-of-primary under `!SafeToHeal` → manual `!NeedsHeal`.
- **P2.T3 — `cnpg buildAssessments`** — ✅ DONE 2026-07-21 (`cnpg_assessments_test.go`): primary never heals; behind-timeline → heal; same-TL streaming → no heal; same-TL stranded (behind+not-streaming+crashloop) → heal.
- **P2.T4 — `cnpg PreAssess` deadlock-breaker flow** (`repair/cnpg_preassess.go:44`) — **NO flow test** (service_test stub is a no-op). The `rm -rf pgdata` orchestration: assert refuse when `AuthorityStatus!="unambiguous"`; refuse on insufficient escrow space with no capture; **no clear if re-triage drift changes the proof hash**; never clears `rec.Authority`; escrow retained on clear failure.
- **P2.T5 — `ParseWsrepRecoverOutput`** (`provider/galera_ops.go:337`) — **NO direct test.** Parses the authoritative recovered position. Assert rejects `ZeroUUID`, `seqno<0`, `seqno>MaxPhantomSeqno`; `LastCommitted` falls back to `seqno`; `Valid` only on clean parse.
- **P2.T6 — `scrapeRecoveredPosition`** (`triage/galera.go:1208`) — **NO test.** A wrong parse returns a too-high seqno that out-ranks the true authority. Assert both log formats, max across lines, `(-1,"")` when absent.
- **P2.T7 — `cnpg buildAuthorityInputs` ReadState branches** (`triage/cnpg.go:673`) — thin. Assert each branch directly, especially `UNKNOWN` (transient) → Unread, **not** AbsentNoData, vs `MISSING`→AbsentNoData.
- **P2.T8 — `galera crossInstanceComparison` divergence/precedence** (`triage/galera.go:527`) — thin. Assert Known seqno-ahead-of-primary → `Diverged`; unread dominates divergence in outcome precedence.
- **P2.T9 — `maybeDeepRecover`/`anyServing` fence gate** (`triage/galera.go:1067/1086`) — **NO test.** A regression here fences a **live** cluster → outage. Assert `isAnythingAlive` true → no fence; empty root password → `anyServing` true → no fence; any `SELECT 1` answer → no fence. (Mirror this for the new CNPG deep-recover gate.)

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
