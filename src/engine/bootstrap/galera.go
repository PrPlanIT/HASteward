package bootstrap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	gcacheThreshold = int64(10000)
	bootstrapLockAn = provider.FenceLockAnnotation // shared fence lock (triage deep-recover sets it too)
	// zeroUUID / maxPhantomSeqno are shared with triage via the provider — aliased
	// here so the lineage-analysis code below reads naturally.
	zeroUUID        = provider.ZeroUUID
	maxPhantomSeqno = provider.MaxPhantomSeqno
)

// wsrepRecoverResult is the provider's fenced-recover result — the fence + recover
// primitives now live on *provider.GaleraProvider (shared with triage). Aliased for
// local brevity.
type wsrepRecoverResult = provider.WsrepRecoverResult

func init() {
	Register("galera", func(ep provider.EngineProvider) (Bootstrapper, error) {
		p, ok := ep.(*provider.GaleraProvider)
		if !ok {
			return nil, fmt.Errorf("galera bootstrapper requires *provider.GaleraProvider, got %T", ep)
		}
		t, err := triage.Get(p)
		if err != nil {
			return nil, fmt.Errorf("galera bootstrapper: %w", err)
		}
		return &galeraBootstrap{p: p, triager: t}, nil
	})
}

// galeraBootstrap implements the Bootstrapper interface for MariaDB Galera clusters.
type galeraBootstrap struct {
	p       *provider.GaleraProvider
	triager triage.Triager
}

func (b *galeraBootstrap) Name() string { return "galera" }

// Bootstrap performs a full Galera cluster bootstrap when all nodes are down.
//
// Flow:
//  1. Triage to assess cluster state
//  2. Paranoid safety gates (refuse if healthy nodes exist, refuse if ambiguous seqno unless --force)
//  3. Identify best bootstrap candidate (highest effective seqno)
//  4. If dryRun: return decision + planned actions without mutation
//  5. Execute: suspend CR -> gen lock -> scale to 0 -> wsrep_recover -> clear stale safe_to_bootstrap ->
//     set safe_to_bootstrap=1 on candidate PVC -> patch CR -> scale up -> wait ready ->
//     cleanup (JSON Patch remove + lock remove) -> resume -> re-triage
//  6. Return typed BootstrapResult
func (b *galeraBootstrap) Bootstrap(ctx context.Context, dryRun bool) (*model.BootstrapResult, error) {
	cfg := b.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()
	clusterRef := model.ObjectRef{
		APIVersion: "k8s.mariadb.com/v1alpha1",
		Kind:       "MariaDB",
		Namespace:  ns,
		Name:       cfg.ClusterName,
	}
	result := &model.BootstrapResult{
		Engine:  "galera",
		Cluster: clusterRef,
	}

	// Check for stale generation lock — blocks if lock < 1 hour old unless --force
	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
	if err == nil {
		lockVal := k8s.GetNestedString(obj, "metadata", "annotations", bootstrapLockAn)
		if lockVal != "" {
			parts := strings.SplitN(lockVal, "@", 2)
			if len(parts) == 2 {
				if ts, terr := time.Parse(time.RFC3339, parts[1]); terr == nil {
					age := time.Since(ts)
					if age < time.Hour {
						common.WarnLog("Stale bootstrap lock detected: %s (age: %s). A previous bootstrap may still be running or failed to clean up.", lockVal, age.Round(time.Second))
						if !cfg.Force {
							return result, fmt.Errorf("ABORT: Stale bootstrap lock from %s (%s ago). Use --force to override", parts[0], age.Round(time.Second))
						}
						common.WarnLog("force=true — overriding stale lock")
					}
				}
			}
		}
	}

	// Phase 1: Triage
	output.Section("Bootstrap Phase 1: Triage")
	triageResult, err := triage.Run(ctx, b.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("triage failed: %w", err)
	}

	// Phase 2: Safety gates
	output.Section("Bootstrap Phase 2: Safety Gates")

	// Gate 1: Must have all nodes down
	if !triageResult.AllNodesDown {
		healthyCount := 0
		for _, a := range triageResult.Assessments {
			if a.IsRunning && a.IsReady {
				healthyCount++
			}
		}
		if healthyCount > 0 {
			result.Decision = model.BootstrapDecision{
				Eligible: false,
				Reason:   fmt.Sprintf("cluster has %d healthy node(s) — bootstrap is only for full cluster down", healthyCount),
			}
			return result, fmt.Errorf("ABORT: Cluster has %d healthy node(s). Bootstrap is only for when ALL nodes are down. Use 'repair' instead", healthyCount)
		}
	}

	// Gate 2: a pre-fence candidate — OR a recoverable belly-up cluster.
	//
	// A Known best-seqno node normally names the bootstrap candidate up front. But
	// a fully belly-up cluster — every mysqld crashed while holding its datadir, so
	// grastate is -1 and NO node has a Known seqno — has none. That is exactly the
	// case the fenced wsrep_recover exists to resolve: it re-derives each node's
	// real uuid:seqno from InnoDB. So rather than abort, proceed into the fence +
	// wsrep_recover and let selectCandidate establish authority from those
	// AUTHORITATIVE positions; the post-recover gate in executeBootstrap then
	// refuses a split-brain/ambiguous result without --force. If wsrep_recover
	// finds nothing recoverable, the candidate guard aborts (after a harmless fence
	// of an already-down cluster) and the deferred rescue restores state.
	bellyUp := triageResult.BestSeqnoNode == nil
	if bellyUp && len(triageResult.Assessments) == 0 {
		result.Decision = model.BootstrapDecision{Eligible: false, Reason: "no nodes found to recover"}
		return result, fmt.Errorf("ABORT: No nodes found. Cannot bootstrap")
	}

	var candidatePod, candidateUUID string
	var candidateSeqno int64
	if !bellyUp {
		candidate := triageResult.BestSeqnoNode
		candidatePod, candidateSeqno, candidateUUID = candidate.Pod, candidate.EffectiveSeqno, candidate.UUID

		// Gate 3: ambiguity (multiple nodes at the same seqno). Hint-based — only
		// meaningful with a Known candidate; belly-up defers this to the
		// authoritative post-recover gate.
		var competitors []string
		for _, a := range triageResult.Assessments {
			if a.Pod != candidatePod && a.EffectiveSeqno == candidateSeqno && candidateSeqno > 0 {
				competitors = append(competitors, a.Pod)
			}
		}
		ambiguous := len(competitors) > 0

		// Gate 4: split-brain detection (pre-fence, hint-based).
		safeToHeal := triageResult.DataComparison.SafeToHeal
		forceRequired := ambiguous || !safeToHeal

		result.Decision = model.BootstrapDecision{
			Eligible:          true,
			Reason:            "all nodes down, bootstrap candidate identified",
			CandidatePod:      candidatePod,
			CandidateSeqno:    candidateSeqno,
			CandidateUUID:     candidateUUID,
			AmbiguityDetected: ambiguous,
			ForceRequired:     forceRequired,
			SafeToProceed:     !forceRequired || cfg.Force,
			Competitors:       competitors,
		}

		if ambiguous {
			common.WarnLog("AMBIGUITY: Multiple nodes at seqno %d: %s and %v", candidateSeqno, candidatePod, competitors)
			if !cfg.Force {
				result.Decision.Reason = fmt.Sprintf("ambiguous: %s and %v all at seqno %d — use --force to pick %s",
					candidatePod, competitors, candidateSeqno, candidatePod)
				return result, fmt.Errorf("ABORT: Ambiguous bootstrap candidate. Multiple nodes at seqno %d. Re-run with --force to select %s",
					candidateSeqno, candidatePod)
			}
			common.WarnLog("force=true — proceeding with %s despite ambiguity", candidatePod)
		}
		if !safeToHeal && !cfg.Force {
			result.Decision.Reason = "split-brain detected — use --force to override"
			return result, fmt.Errorf("ABORT: Split-brain detected. Re-run with --force to override")
		}
		output.Success("Bootstrap candidate: %s (seqno: %d, uuid: %s)", candidatePod, candidateSeqno, candidateUUID)
	} else {
		common.WarnLog("Belly-up cluster: no Known seqno on any node (grastate -1). Fencing + wsrep_recover will establish authority from authoritative positions.")
		result.Decision = model.BootstrapDecision{
			Eligible:      true,
			Reason:        "belly-up — authority to be established by fenced wsrep_recover",
			SafeToProceed: true, // the post-recover gate enforces --force on a real split-brain
		}
		output.Info("Belly-up recovery: authoritative candidate will be chosen from wsrep_recover results")
	}

	// Build planned actions
	stsRef := model.ObjectRef{Kind: "StatefulSet", Namespace: ns, Name: cfg.ClusterName}
	crRef := clusterRef

	markDesc, patchDesc := "<recovered candidate>", "Patch CR forceClusterBootstrapInPod=<recovered candidate>"
	if candidatePod != "" {
		markDesc = candidatePod
		patchDesc = "Patch CR forceClusterBootstrapInPod=" + candidatePod
	}

	result.ActionsPlanned = []model.BootstrapAction{
		{Phase: model.PhaseSuspend, Description: "Suspend MariaDB CR", Resource: &crRef},
		{Phase: model.PhaseGenLock, Description: "Set generation lock annotation", Resource: &crRef},
		{Phase: model.PhaseScaleDown, Description: "Scale StatefulSet to 0", Resource: &stsRef},
		{Phase: model.PhaseWsrepRecover, Description: "Run wsrep_recover on all PVCs"},
		{Phase: model.PhaseSafeBootClear, Description: "Clear stale safe_to_bootstrap flags"},
		{Phase: model.PhaseBootstrapMark, Description: fmt.Sprintf("Set safe_to_bootstrap=1 on %s PVC", markDesc)},
		{Phase: model.PhaseClusterPatch, Description: patchDesc, Resource: &crRef},
		{Phase: model.PhaseScaleUp, Description: fmt.Sprintf("Scale StatefulSet to %d", b.p.Replicas()), Resource: &stsRef},
		{Phase: model.PhaseWaitReady, Description: "Wait for all pods Ready"},
		{Phase: model.PhaseCleanup, Description: "Remove forceClusterBootstrapInPod, recovery status, and generation lock", Resource: &crRef},
		{Phase: model.PhaseResume, Description: "Resume MariaDB CR", Resource: &crRef},
		{Phase: model.PhaseVerify, Description: "Re-triage to verify cluster health"},
	}

	// Dry run: return plan without executing
	if dryRun {
		output.Info("DRY RUN — returning planned actions without executing")
		return result, nil
	}

	// Phase 3: Execute
	output.Section("Bootstrap Phase 3: Execute")
	if err := b.executeBootstrap(ctx, candidatePod, bellyUp, triageResult.Assessments, result); err != nil {
		return result, err
	}

	return result, nil
}

func (b *galeraBootstrap) executeBootstrap(ctx context.Context, candidatePod string, bellyUp bool, assessments []model.InstanceAssessment, result *model.BootstrapResult) error {
	cfg := b.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()
	originalReplicas := int32(b.p.Replicas())

	// Capture SA before pods are deleted
	sa := "default"
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: b.p.PodSelector(),
	})
	if err == nil {
		sa = k8s.ServiceAccountFromPods(pods.Items)
	}

	suspended := false
	scaledDown := false
	genLocked := false
	safeBootMarked := false // safe_to_bootstrap=1 written on the candidate PVC (STEP 6)
	forcePatched := false   // forceClusterBootstrapInPod set on the CR (STEP 7)

	rescue := func() {
		// Undo the AUTHORITY mutations first, while the pods are still scaled to 0,
		// so a failed run never resumes the operator with a live directive to
		// bootstrap from a candidate we just abandoned. Symmetric with STEP 6/7.
		if forcePatched {
			clearPatch := `[{"op":"remove","path":"/spec/galera/recovery/forceClusterBootstrapInPod"}]`
			_, _ = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
				ctx, cfg.ClusterName, types.JSONPatchType, []byte(clearPatch), metav1.PatchOptions{})
		}
		if safeBootMarked {
			revertScript := `set -e
test -f /var/lib/mysql/grastate.dat || exit 0
sed -i 's/safe_to_bootstrap: 1/safe_to_bootstrap: 0/' /var/lib/mysql/grastate.dat
echo "reverted safe_to_bootstrap to 0"`
			helperName := fmt.Sprintf("%s-safeboot-revert-%d", cfg.ClusterName, time.Now().Unix())
			_ = b.p.RunHelperPod(ctx, helperName, b.p.DataPVCName(candidatePod), "/var/lib/mysql", revertScript, sa)
		}
		// Remove generation lock on failure
		if genLocked {
			lockClearPatch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, bootstrapLockAn)
			_, _ = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
				ctx, cfg.ClusterName, types.MergePatchType, []byte(lockClearPatch), metav1.PatchOptions{})
		}
		if scaledDown {
			_ = b.p.ScaleStatefulSet(ctx, originalReplicas)
		}
		if suspended {
			_ = b.p.ResumeCR(ctx)
		}
		if suspended || scaledDown || genLocked {
			common.WarnLog("BOOTSTRAP FAILED — rolled back (any authority markers cleared, scale restored, CR resumed).")
		}
	}

	markAction := func(phase string) {
		for i := range result.ActionsTaken {
			if result.ActionsTaken[i].Phase == phase {
				result.ActionsTaken[i].Completed = true
				return
			}
		}
	}

	// Copy planned to taken
	result.ActionsTaken = make([]model.BootstrapAction, len(result.ActionsPlanned))
	copy(result.ActionsTaken, result.ActionsPlanned)

	// STEP 1: Suspend CR
	common.InfoLog("STEP 1: Suspending MariaDB CR")
	if err := b.p.SuspendCR(ctx); err != nil {
		return fmt.Errorf("failed to suspend CR: %w", err)
	}
	suspended = true
	markAction(model.PhaseSuspend)
	common.Sleep(3 * time.Second)

	// STEP 2: Set generation lock annotation
	common.InfoLog("STEP 2: Setting generation lock annotation")
	lockValue := fmt.Sprintf("%s@%s", candidatePod, time.Now().UTC().Format(time.RFC3339))
	lockPatch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, bootstrapLockAn, lockValue)
	_, err = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(lockPatch), metav1.PatchOptions{})
	if err != nil {
		rescue()
		return fmt.Errorf("failed to set generation lock: %w", err)
	}
	genLocked = true
	markAction(model.PhaseGenLock)

	// STEP 3: Scale to 0
	common.InfoLog("STEP 3: Scaling StatefulSet to 0")
	if err := b.p.ScaleStatefulSet(ctx, 0); err != nil {
		rescue()
		return fmt.Errorf("failed to scale StatefulSet to 0: %w", err)
	}
	scaledDown = true
	markAction(model.PhaseScaleDown)

	// Clear operator recovery pods before waiting: they carry the cluster label, so
	// a hung one keeps WaitPodsTerminated blocked — and with the CR suspended,
	// nothing else deletes it — spuriously failing an otherwise valid fence. Matches
	// triage's deepRecover fence ordering.
	b.p.DeleteRecoveryPods(ctx)

	// Fence must be CONFIRMED complete before recover — a helper mariadbd must never
	// open a datadir a real mysqld still holds. Abort (and rescue) if pods survive.
	if err := b.p.WaitPodsTerminated(ctx, cfg.DeleteTimeout); err != nil {
		rescue()
		return err
	}

	// STEP 4: Run wsrep_recover on all PVCs
	common.InfoLog("STEP 4: Running wsrep_recover on all PVCs")
	recoveredResults := make(map[string]wsrepRecoverResult)
	for _, a := range assessments {
		rr, rerr := b.p.RunWsrepRecover(ctx, a.Pod, sa)
		if rerr != nil {
			// Skip a failed recover — never fabricate a hint-backed fallback. An
			// invalid entry can't nominate authority (isAuthoritativeRecover), but a
			// fabricated seqno still skewed the gcache IST-gap below; excluding the
			// node entirely is correct and mirrors triage's deepRecover.
			common.WarnLog("wsrep_recover failed for %s: %v — excluding this node from candidate/lineage selection", a.Pod, rerr)
			continue
		}
		common.InfoLog("wsrep_recover %s: uuid=%s seqno=%d lastCommitted=%d", a.Pod, rr.UUID, rr.Seqno, rr.LastCommitted)
		recoveredResults[a.Pod] = rr
	}
	markAction(model.PhaseWsrepRecover)

	// Build lineage groups and select best candidate
	candidatePod, err = b.selectCandidate(candidatePod, assessments, recoveredResults, result)
	if err != nil {
		rescue()
		return err
	}

	// STEP 4.5: Authority gate on the AUTHORITATIVE positions (belly-up path).
	// wsrep_recover has now established each node's real uuid:seqno. If that reveals
	// a genuine split-brain (>1 lineage group), refuse to bootstrap without --force:
	// the fence + recover was to LEARN the truth, and discarding a divergent lineage
	// is a human decision. A clean single lineage proceeds. (The Known-candidate
	// path already gated split-brain pre-fence, on hints.) rescue() restores state.
	if bellyUp && !cfg.Force && len(result.Decision.LineageGroups) > 1 {
		rescue()
		return fmt.Errorf("ABORT: wsrep_recover confirms split-brain across %d lineages — authoritative positions reported above. "+
			"Re-run with --force to bootstrap from the majority lineage (candidate %s)", len(result.Decision.LineageGroups), candidatePod)
	}

	// Candidate validation guard — ensure selectCandidate returned a pod that exists in assessments
	found := false
	for _, a := range assessments {
		if a.Pod == candidatePod {
			found = true
			break
		}
	}
	if !found {
		rescue()
		return fmt.Errorf("candidate pod %s not found in assessments — stale reference", candidatePod)
	}

	// STEP 4b: gcache IST loop detection
	maxSeqno := int64(0)
	for _, rr := range recoveredResults {
		if rr.Seqno > maxSeqno {
			maxSeqno = rr.Seqno
		}
	}
	for pod, rr := range recoveredResults {
		if pod == candidatePod {
			continue
		}
		gap := maxSeqno - rr.Seqno
		if gap > gcacheThreshold && rr.Seqno >= 0 {
			common.WarnLog("Node %s is %d transactions behind — exceeds likely gcache window. Removing galera.cache to force SST.", pod, gap)
			clearScript := `test -f /var/lib/mysql/galera.cache && rm /var/lib/mysql/galera.cache && echo "galera.cache removed — SST will be forced" || echo "no galera.cache present"`
			helperName := fmt.Sprintf("%s-gcache-clear-%d", cfg.ClusterName, time.Now().Unix())
			_ = b.p.RunHelperPod(ctx, helperName, b.p.DataPVCName(pod), "/var/lib/mysql", clearScript, sa)
		}
	}

	// STEP 5: Clear stale safe_to_bootstrap flags on non-candidate nodes
	common.InfoLog("STEP 5: Clearing stale safe_to_bootstrap flags")
	for _, a := range assessments {
		if a.Pod == candidatePod {
			continue
		}
		if a.SafeToBootstrap == "1" {
			common.WarnLog("Clearing stale safe_to_bootstrap on %s", a.Pod)
			clearScript := `set -e
test -f /var/lib/mysql/grastate.dat || exit 0
grep -q "safe_to_bootstrap: 1" /var/lib/mysql/grastate.dat && \
sed -i 's/safe_to_bootstrap: 1/safe_to_bootstrap: 0/' /var/lib/mysql/grastate.dat && \
echo "cleared" || echo "already clean"
`
			helperName := fmt.Sprintf("%s-safe-clear-%d", cfg.ClusterName, time.Now().Unix())
			if serr := b.p.RunHelperPod(ctx, helperName, b.p.DataPVCName(a.Pod), "/var/lib/mysql", clearScript, sa); serr != nil {
				common.WarnLog("Failed to clear safe_to_bootstrap on %s: %v", a.Pod, serr)
			}
		}
	}
	markAction(model.PhaseSafeBootClear)

	// STEP 6: Set safe_to_bootstrap=1 on candidate PVC + remove galera.cache for fresh peer discovery
	common.InfoLog("STEP 6: Setting safe_to_bootstrap=1 on %s", candidatePod)
	storagePVC := b.p.DataPVCName(candidatePod)
	helperName := fmt.Sprintf("%s-bootstrap-%d", cfg.ClusterName, time.Now().Unix())

	bootstrapScript := `set -e
echo "=== Current grastate.dat ==="
cat /var/lib/mysql/grastate.dat 2>/dev/null || echo "not found"
echo ""
echo "=== Preserving grastate.dat ==="
cp /var/lib/mysql/grastate.dat /var/lib/mysql/grastate.dat.pre-bootstrap 2>/dev/null || echo "nothing to preserve"
echo "=== Setting safe_to_bootstrap: 1 ==="
sed -i 's/safe_to_bootstrap: 0/safe_to_bootstrap: 1/' /var/lib/mysql/grastate.dat
echo "=== Updated grastate.dat ==="
cat /var/lib/mysql/grastate.dat
echo "=== Removing galera.cache for fresh peer discovery ==="
test -f /var/lib/mysql/galera.cache && rm /var/lib/mysql/galera.cache && echo "removed" || echo "not present"
echo "=== Done ==="
`
	if err := b.p.RunHelperPod(ctx, helperName, storagePVC, "/var/lib/mysql", bootstrapScript, sa); err != nil {
		rescue()
		return fmt.Errorf("failed to set safe_to_bootstrap: %w", err)
	}
	safeBootMarked = true // rescue must now revert this on any later failure
	markAction(model.PhaseBootstrapMark)

	// STEP 7: Patch CR with forceClusterBootstrapInPod
	common.InfoLog("STEP 7: Patching CR forceClusterBootstrapInPod=%s", candidatePod)
	patchJSON := fmt.Sprintf(`{"spec":{"galera":{"recovery":{"forceClusterBootstrapInPod":"%s"}}}}`, candidatePod)
	_, err = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patchJSON), metav1.PatchOptions{})
	if err != nil {
		rescue()
		return fmt.Errorf("failed to patch CR: %w", err)
	}
	forcePatched = true // rescue must now clear forceClusterBootstrapInPod on any later failure
	markAction(model.PhaseClusterPatch)

	// STEP 8: Scale back up
	common.InfoLog("STEP 8: Scaling StatefulSet to %d", originalReplicas)
	b.p.DeleteRecoveryPods(ctx)
	common.Sleep(2 * time.Second)

	if err := b.p.ScaleStatefulSet(ctx, originalReplicas); err != nil {
		rescue()
		return fmt.Errorf("failed to scale StatefulSet back up: %w", err)
	}
	scaledDown = false
	markAction(model.PhaseScaleUp)

	// STEP 9: Wait for all pods ready (soft timeout: 15 minutes)
	common.InfoLog("STEP 9: Waiting for all pods to become ready")
	b.waitForAllReady(ctx)
	markAction(model.PhaseWaitReady)

	// STEP 10: Cleanup — remove forceClusterBootstrapInPod via JSON Patch + remove generation lock
	common.InfoLog("STEP 10: Cleaning up forceClusterBootstrapInPod")
	clearPatch := `[{"op":"remove","path":"/spec/galera/recovery/forceClusterBootstrapInPod"}]`
	for attempt := 1; attempt <= 5; attempt++ {
		_, err = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
			ctx, cfg.ClusterName, types.JSONPatchType, []byte(clearPatch), metav1.PatchOptions{})
		if err != nil {
			if apierrors.IsInvalid(err) || apierrors.IsNotFound(err) {
				// Field doesn't exist or path invalid — already clean
				err = nil
				break
			}
			common.WarnLog("Cleanup patch attempt %d failed: %v", attempt, err)
			common.Sleep(2 * time.Second)
			continue
		}
		// Verify the field is actually gone
		obj, verr := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
		if verr == nil && !k8s.HasNestedField(obj, "spec", "galera", "recovery", "forceClusterBootstrapInPod") {
			break
		}
		if verr == nil {
			common.WarnLog("Cleanup attempt %d: field still present, retrying", attempt)
		}
		common.Sleep(2 * time.Second)
	}
	if err != nil {
		common.WarnLog("Failed to clear forceClusterBootstrapInPod after retries: %v\nManual cleanup may be required.", err)
	}

	// Clear stale operator recovery status — prevents operator from
	// re-entering bootstrap mode when CR is resumed
	common.InfoLog("Clearing operator recovery status (status.galeraRecovery)")
	recoveryClearPatch := `{"status":{"galeraRecovery":null}}`
	for attempt := 1; attempt <= 5; attempt++ {
		_, serr := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).
			Patch(ctx, cfg.ClusterName, types.MergePatchType,
				[]byte(recoveryClearPatch), metav1.PatchOptions{}, "status")
		if serr == nil {
			break
		}
		common.WarnLog("status.galeraRecovery clear attempt %d failed: %v", attempt, serr)
		common.Sleep(2 * time.Second)
	}
	// Verify the field is actually gone
	statusObj, verr := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
	if verr == nil && k8s.HasNestedField(statusObj, "status", "galeraRecovery") {
		common.WarnLog("status.galeraRecovery still present after clear — operator may re-populate it")
	}

	// Remove generation lock
	lockClearPatch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, bootstrapLockAn)
	_, _ = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(lockClearPatch), metav1.PatchOptions{})
	genLocked = false
	markAction(model.PhaseCleanup)

	// STEP 11: Resume CR
	common.InfoLog("STEP 11: Resuming MariaDB CR")
	if err := b.p.ResumeCR(ctx); err != nil {
		common.WarnLog("Failed to resume CR: %v — manual resume may be required", err)
	}
	suspended = false
	markAction(model.PhaseResume)

	// STEP 12: Re-triage
	output.Section("Bootstrap Phase 4: Verify")
	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
	if err == nil {
		b.p.SetMariaDB(obj)
	}

	postTriage, _ := triage.Run(ctx, b.triager, engine.NopSink{})
	markAction(model.PhaseVerify)

	if postTriage != nil {
		healthy := postTriage.ReadyCount == postTriage.TotalCount
		result.FinalHealth = &model.ClusterHealthSummary{
			ReadyCount: postTriage.ReadyCount,
			TotalCount: postTriage.TotalCount,
			Healthy:    healthy,
		}
		if healthy {
			output.Success("Bootstrap complete — cluster is healthy (%d/%d ready)", postTriage.ReadyCount, postTriage.TotalCount)
		} else {
			output.Warn("Bootstrap complete — cluster may need time (%d/%d ready)", postTriage.ReadyCount, postTriage.TotalCount)
		}
	}

	return nil
}

// selectCandidate applies wsrep_recover results and lineage-group analysis
// to choose the best bootstrap candidate. Returns the (possibly overridden)
// candidate pod name.
func (b *galeraBootstrap) selectCandidate(
	originalCandidate string,
	assessments []model.InstanceAssessment,
	recovered map[string]wsrepRecoverResult,
	result *model.BootstrapResult,
) (string, error) {
	cfg := b.p.Config()

	groups := buildLineageGroups(recovered)
	result.Decision.LineageGroups = groups

	if len(groups) == 0 {
		common.WarnLog("wsrep_recover produced no valid results — using original candidate %s", originalCandidate)
		return originalCandidate, nil
	}

	// Log lineage groups
	for _, g := range groups {
		common.InfoLog("Lineage group UUID=%s: %d members %v, maxSeqno=%d, maxCommitted=%d, bestNode=%s",
			g.UUID, len(g.Members), g.Members, g.MaxSeqno, g.MaxCommitted, g.BestNode)
	}

	majorityGroup := groups[0]
	candidatePod := originalCandidate
	candidateRR := recovered[originalCandidate]

	// Check if original candidate is in the majority group
	candidateInMajority := false
	for _, m := range majorityGroup.Members {
		if m == originalCandidate {
			candidateInMajority = true
			break
		}
	}

	if len(groups) > 1 {
		// Multiple lineage groups = split-brain recovery scenario
		common.WarnLog("Split-brain recovery detected: %d lineage groups", len(groups))
		for _, g := range groups {
			common.WarnLog("  UUID=%s: %d members, maxSeqno=%d, bestNode=%s", g.UUID, len(g.Members), g.MaxSeqno, g.BestNode)
		}

		if !candidateInMajority && candidateRR.UUID != majorityGroup.UUID {
			// Candidate is from a minority lineage
			common.WarnLog("Candidate %s belongs to minority lineage UUID=%s (%d members). Majority lineage UUID=%s has %d members.",
				originalCandidate, candidateRR.UUID, len(recovered)-len(majorityGroup.Members), majorityGroup.UUID, len(majorityGroup.Members))

			if !cfg.Force {
				// Switch to majority group's best node
				candidatePod = majorityGroup.BestNode
				common.WarnLog("Switching candidate to %s from majority lineage. Use --force to bootstrap minority lineage %s instead.",
					candidatePod, originalCandidate)
			} else {
				common.WarnLog("force=true — keeping minority lineage candidate %s despite majority lineage having more members", originalCandidate)
			}
		}
	}

	// Within the selected lineage group, find the absolute best node
	// Priority: highest seqno, then highest lastCommitted
	// Seed from nothing and let the first AUTHORITATIVE recover win — never seed
	// from the original candidate's own result, which may be a failed/hint-backed
	// (Valid:false) entry whose hint seqno would out-rank a genuine recovered
	// position and suppress a legitimate override.
	bestPod := ""
	bestSeqno := int64(-1)
	bestCommitted := int64(-1)
	bestUUID := ""

	for pod, rr := range recovered {
		if !isAuthoritativeRecover(rr) {
			continue // a failed/hint/never-joined recover may not drive authority
		}

		// If we're preferring majority lineage (default), only consider nodes in that UUID
		if !cfg.Force && len(groups) > 1 && rr.UUID != majorityGroup.UUID {
			continue
		}

		if bestPod == "" || rr.Seqno > bestSeqno || (rr.Seqno == bestSeqno && rr.LastCommitted > bestCommitted) {
			bestPod = pod
			bestSeqno = rr.Seqno
			bestCommitted = rr.LastCommitted
			bestUUID = rr.UUID
		}
	}

	// No authoritative recover in the selected lineage — keep the original
	// candidate rather than override on nothing.
	if bestPod == "" {
		return candidatePod, nil
	}

	if bestPod != originalCandidate {
		// Find original grastate seqno for logging
		origGrastate := int64(0)
		for _, a := range assessments {
			if a.Pod == originalCandidate {
				origGrastate = a.EffectiveSeqno
				break
			}
		}
		common.WarnLog("wsrep_recover override: switching candidate from %s (grastate seqno %d) to %s (recovered seqno %d, uuid %s)",
			originalCandidate, origGrastate, bestPod, bestSeqno, bestUUID)
		result.Decision.WsrepRecoverApplied = true
		result.Decision.OriginalCandidate = originalCandidate
		result.Decision.CandidatePod = bestPod
		result.Decision.CandidateSeqno = bestSeqno
		result.Decision.CandidateUUID = bestUUID
		return bestPod, nil
	}

	return candidatePod, nil
}

// isAuthoritativeRecover reports whether a wsrep_recover result may drive cluster
// authority. Only a result that is Valid (a real --wsrep-recover that parsed to a
// concrete position — NOT a failed/hint-backed fallback), carries a real cluster
// UUID (not zero/empty/"unknown"), and holds a sane seqno (0..maxPhantomSeqno)
// qualifies. This is the single predicate that keeps HINTS from nominating or
// overriding the bootstrap target — the same "hints never establish authority"
// rule triage enforces for seqnos and UUIDs.
func isAuthoritativeRecover(rr wsrepRecoverResult) bool {
	return rr.Valid &&
		rr.UUID != zeroUUID && rr.UUID != "" && rr.UUID != "unknown" &&
		rr.Seqno >= 0 && rr.Seqno <= maxPhantomSeqno
}

// buildLineageGroups partitions wsrep_recover results into lineage groups keyed by
// cluster UUID, sorted by member count (descending) then MaxSeqno (descending) so
// groups[0] is the majority lineage. Only authoritative results participate (see
// isAuthoritativeRecover) — a failed/hint-backed, never-joined, or phantom-seqno
// result cannot form a lineage. More than one group means a genuine split-brain —
// the signal the belly-up authority gate refuses without --force.
func buildLineageGroups(recovered map[string]wsrepRecoverResult) []model.LineageGroup {
	groupMap := make(map[string]*model.LineageGroup)
	for pod, rr := range recovered {
		if !isAuthoritativeRecover(rr) {
			common.WarnLog("Ignoring recovery result for %s: not an authoritative lineage (uuid=%q seqno=%d valid=%v)", pod, rr.UUID, rr.Seqno, rr.Valid)
			continue
		}

		g, ok := groupMap[rr.UUID]
		if !ok {
			g = &model.LineageGroup{UUID: rr.UUID}
			groupMap[rr.UUID] = g
		}
		g.Members = append(g.Members, pod)
		if rr.Seqno > g.MaxSeqno || (rr.Seqno == g.MaxSeqno && rr.LastCommitted > g.MaxCommitted) {
			g.MaxSeqno = rr.Seqno
			g.MaxCommitted = rr.LastCommitted
			g.BestNode = pod
		}
	}

	var groups []model.LineageGroup
	for _, g := range groupMap {
		sort.Strings(g.Members)
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Members) != len(groups[j].Members) {
			return len(groups[i].Members) > len(groups[j].Members)
		}
		return groups[i].MaxSeqno > groups[j].MaxSeqno
	})
	return groups
}

// waitForAllReady waits for all StatefulSet pods to become Running and Ready.
// Soft timeout of 15 minutes — continues to verify step if not all ready.
func (b *galeraBootstrap) waitForAllReady(ctx context.Context) {
	cfg := b.p.Config()
	expected := int(b.p.Replicas())
	// 90 iterations × 10s = 15 minutes soft timeout
	if k8s.WaitAllReady(ctx, cfg.Namespace, b.p.PodSelector(), expected, 90, 10, "mariadb") {
		common.InfoLog("All %d pods are Running and Ready", expected)
		return
	}
	common.WarnLog("Not all pods became ready within 15 minute timeout — continuing to verify step")
}

// ptr returns a pointer to the given value.
