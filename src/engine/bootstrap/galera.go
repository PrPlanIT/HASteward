package bootstrap

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	zeroUUID        = "00000000-0000-0000-0000-000000000000"
	gcacheThreshold = int64(10000)
	maxPhantomSeqno = int64(1e12)
	bootstrapLockAn = "hasteward.prplanit.com/bootstrap-lock"
)

var (
	reRecoveredPos = regexp.MustCompile(`Recovered position:\s*([0-9a-fA-F-]+):([0-9-]+)`)
	reLastCommit   = regexp.MustCompile(`Last committed:\s*([0-9]+)`)
)

// wsrepRecoverResult holds the parsed output from mariadbd --wsrep-recover.
type wsrepRecoverResult struct {
	UUID          string
	Seqno         int64
	LastCommitted int64
	Valid         bool
}

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

	// Gate 2: Must have a best candidate
	if triageResult.BestSeqnoNode == nil {
		result.Decision = model.BootstrapDecision{
			Eligible: false,
			Reason:   "no bootstrap candidate found — could not determine best seqno node",
		}
		return result, fmt.Errorf("ABORT: Could not determine best seqno node. Cannot bootstrap")
	}

	candidate := triageResult.BestSeqnoNode
	candidateSeqno := candidate.EffectiveSeqno

	// Gate 3: Check for ambiguity (multiple nodes at same seqno)
	var competitors []string
	for _, a := range triageResult.Assessments {
		if a.Pod != candidate.Pod && a.EffectiveSeqno == candidateSeqno && candidateSeqno > 0 {
			competitors = append(competitors, a.Pod)
		}
	}
	ambiguous := len(competitors) > 0

	// Gate 4: Split-brain detection
	safeToHeal := triageResult.DataComparison.SafeToHeal

	forceRequired := ambiguous || !safeToHeal
	safeToProceed := !forceRequired || cfg.Force

	result.Decision = model.BootstrapDecision{
		Eligible:          true,
		Reason:            "all nodes down, bootstrap candidate identified",
		CandidatePod:      candidate.Pod,
		CandidateSeqno:    candidateSeqno,
		CandidateUUID:     candidate.UUID,
		AmbiguityDetected: ambiguous,
		ForceRequired:     forceRequired,
		SafeToProceed:     safeToProceed,
		Competitors:       competitors,
	}

	if ambiguous {
		common.WarnLog("AMBIGUITY: Multiple nodes at seqno %d: %s and %v",
			candidateSeqno, candidate.Pod, competitors)
		if !cfg.Force {
			result.Decision.Reason = fmt.Sprintf("ambiguous: %s and %v all at seqno %d — use --force to pick %s",
				candidate.Pod, competitors, candidateSeqno, candidate.Pod)
			return result, fmt.Errorf("ABORT: Ambiguous bootstrap candidate. Multiple nodes at seqno %d. Re-run with --force to select %s",
				candidateSeqno, candidate.Pod)
		}
		common.WarnLog("force=true — proceeding with %s despite ambiguity", candidate.Pod)
	}

	if !safeToHeal && !cfg.Force {
		result.Decision.Reason = "split-brain detected — use --force to override"
		return result, fmt.Errorf("ABORT: Split-brain detected. Re-run with --force to override")
	}

	output.Success("Bootstrap candidate: %s (seqno: %d, uuid: %s)", candidate.Pod, candidateSeqno, candidate.UUID)

	// Build planned actions
	stsRef := model.ObjectRef{Kind: "StatefulSet", Namespace: ns, Name: cfg.ClusterName}
	crRef := clusterRef

	result.ActionsPlanned = []model.BootstrapAction{
		{Phase: model.PhaseSuspend, Description: "Suspend MariaDB CR", Resource: &crRef},
		{Phase: model.PhaseGenLock, Description: "Set generation lock annotation", Resource: &crRef},
		{Phase: model.PhaseScaleDown, Description: "Scale StatefulSet to 0", Resource: &stsRef},
		{Phase: model.PhaseWsrepRecover, Description: "Run wsrep_recover on all PVCs"},
		{Phase: model.PhaseSafeBootClear, Description: "Clear stale safe_to_bootstrap flags"},
		{Phase: model.PhaseBootstrapMark, Description: fmt.Sprintf("Set safe_to_bootstrap=1 on %s PVC", candidate.Pod)},
		{Phase: model.PhaseClusterPatch, Description: "Patch CR forceClusterBootstrapInPod=" + candidate.Pod, Resource: &crRef},
		{Phase: model.PhaseScaleUp, Description: fmt.Sprintf("Scale StatefulSet to %d", b.p.Replicas()), Resource: &stsRef},
		{Phase: model.PhaseWaitReady, Description: "Wait for all pods Ready"},
		{Phase: model.PhaseCleanup, Description: "Remove forceClusterBootstrapInPod and generation lock", Resource: &crRef},
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
	if err := b.executeBootstrap(ctx, candidate.Pod, triageResult.Assessments, result); err != nil {
		return result, err
	}

	return result, nil
}

func (b *galeraBootstrap) executeBootstrap(ctx context.Context, candidatePod string, assessments []model.InstanceAssessment, result *model.BootstrapResult) error {
	cfg := b.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()
	originalReplicas := int32(b.p.Replicas())

	// Capture SA before pods are deleted
	sa := "default"
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", cfg.ClusterName),
	})
	if err == nil {
		sa = k8s.ServiceAccountFromPods(pods.Items)
	}

	suspended := false
	scaledDown := false
	genLocked := false

	rescue := func() {
		// Remove generation lock on failure
		if genLocked {
			lockClearPatch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, bootstrapLockAn)
			_, _ = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
				ctx, cfg.ClusterName, types.MergePatchType, []byte(lockClearPatch), metav1.PatchOptions{})
		}
		if scaledDown {
			_ = b.scaleStatefulSet(ctx, originalReplicas)
		}
		if suspended {
			_ = b.resumeCR(ctx)
		}
		if suspended || scaledDown || genLocked {
			common.WarnLog("BOOTSTRAP FAILED. CR resumed, scale restored, lock cleared.")
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
	if err := b.suspendCR(ctx); err != nil {
		return fmt.Errorf("failed to suspend CR: %w", err)
	}
	suspended = true
	markAction(model.PhaseSuspend)
	time.Sleep(3 * time.Second)

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
	if err := b.scaleStatefulSet(ctx, 0); err != nil {
		rescue()
		return fmt.Errorf("failed to scale StatefulSet to 0: %w", err)
	}
	scaledDown = true
	markAction(model.PhaseScaleDown)

	// Wait for all pods to terminate
	deleteTimeout := cfg.DeleteTimeout
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}
	for i := 0; i < deleteTimeout/5; i++ {
		pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", cfg.ClusterName),
		})
		if err != nil || len(pods.Items) == 0 {
			common.InfoLog("All pods terminated")
			break
		}
		time.Sleep(5 * time.Second)
	}

	// STEP 4: Run wsrep_recover on all PVCs
	common.InfoLog("STEP 4: Running wsrep_recover on all PVCs")
	recoveredResults := make(map[string]wsrepRecoverResult)
	for _, a := range assessments {
		rr, rerr := b.runWsrepRecover(ctx, a.Pod, sa)
		if rerr != nil {
			common.WarnLog("wsrep_recover failed for %s: %v — falling back to grastate seqno %d", a.Pod, rerr, a.EffectiveSeqno)
			recoveredResults[a.Pod] = wsrepRecoverResult{
				UUID:          a.UUID,
				Seqno:         a.EffectiveSeqno,
				LastCommitted: a.EffectiveSeqno,
				Valid:         false,
			}
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
			_ = b.runHelperPod(ctx, helperName, fmt.Sprintf("storage-%s", pod), "/var/lib/mysql", clearScript, sa)
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
			if serr := b.runHelperPod(ctx, helperName, fmt.Sprintf("storage-%s", a.Pod), "/var/lib/mysql", clearScript, sa); serr != nil {
				common.WarnLog("Failed to clear safe_to_bootstrap on %s: %v", a.Pod, serr)
			}
		}
	}
	markAction(model.PhaseSafeBootClear)

	// STEP 6: Set safe_to_bootstrap=1 on candidate PVC + remove galera.cache for fresh peer discovery
	common.InfoLog("STEP 6: Setting safe_to_bootstrap=1 on %s", candidatePod)
	storagePVC := fmt.Sprintf("storage-%s", candidatePod)
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
	if err := b.runHelperPod(ctx, helperName, storagePVC, "/var/lib/mysql", bootstrapScript, sa); err != nil {
		rescue()
		return fmt.Errorf("failed to set safe_to_bootstrap: %w", err)
	}
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
	markAction(model.PhaseClusterPatch)

	// STEP 8: Scale back up
	common.InfoLog("STEP 8: Scaling StatefulSet to %d", originalReplicas)
	b.deleteRecoveryPods(ctx)
	time.Sleep(2 * time.Second)

	if err := b.scaleStatefulSet(ctx, originalReplicas); err != nil {
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
			time.Sleep(2 * time.Second)
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
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		common.WarnLog("Failed to clear forceClusterBootstrapInPod after retries: %v\nManual cleanup may be required.", err)
	}

	// Remove generation lock
	lockClearPatch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, bootstrapLockAn)
	_, _ = c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(ns).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(lockClearPatch), metav1.PatchOptions{})
	genLocked = false
	markAction(model.PhaseCleanup)

	// STEP 11: Resume CR
	common.InfoLog("STEP 11: Resuming MariaDB CR")
	if err := b.resumeCR(ctx); err != nil {
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

	// Build lineage groups keyed by UUID
	groupMap := make(map[string]*model.LineageGroup)
	for pod, rr := range recovered {
		// Skip zero UUID (node never joined cluster)
		if rr.UUID == zeroUUID || rr.UUID == "" {
			common.WarnLog("Ignoring recovery result for %s: zero/empty UUID (node never joined cluster)", pod)
			continue
		}
		// Skip phantom seqnos
		if rr.Seqno < 0 || rr.Seqno > maxPhantomSeqno {
			common.WarnLog("Ignoring recovery seqno for %s: %d (phantom/corrupt)", pod, rr.Seqno)
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

	// Collect and sort groups by member count descending, then MaxSeqno descending
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
	bestPod := candidatePod
	bestSeqno := recovered[candidatePod].Seqno
	bestCommitted := recovered[candidatePod].LastCommitted
	bestUUID := recovered[candidatePod].UUID

	for pod, rr := range recovered {
		if rr.UUID == zeroUUID || rr.UUID == "" || rr.Seqno < 0 || rr.Seqno > maxPhantomSeqno {
			continue
		}

		// If we're preferring majority lineage (default), only consider nodes in that UUID
		if !cfg.Force && len(groups) > 1 && rr.UUID != majorityGroup.UUID {
			continue
		}

		if rr.Seqno > bestSeqno || (rr.Seqno == bestSeqno && rr.LastCommitted > bestCommitted) {
			bestPod = pod
			bestSeqno = rr.Seqno
			bestCommitted = rr.LastCommitted
			bestUUID = rr.UUID
		}
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

// runWsrepRecover runs mariadbd --wsrep-recover on a PVC via a helper pod
// and parses the recovered position.
func (b *galeraBootstrap) runWsrepRecover(ctx context.Context, podName, sa string) (wsrepRecoverResult, error) {
	cfg := b.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	image := b.p.Image()
	if image == "" {
		return wsrepRecoverResult{}, fmt.Errorf("cannot determine MariaDB image from CR spec")
	}

	pvcName := fmt.Sprintf("storage-%s", podName)
	helperName := fmt.Sprintf("%s-wsrep-%s-%d", cfg.ClusterName, podName, time.Now().Unix())

	rootUser := int64(0)
	deadline := int64(120)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperName,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "heal-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:        corev1.RestartPolicyNever,
			ServiceAccountName:   sa,
			ActiveDeadlineSeconds: &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &rootUser,
			},
			Containers: []corev1.Container{{
				Name:    "wsrep-recover",
				Image:   image,
				Command: []string{"sh", "-c", "mariadbd --wsrep-recover --datadir=/var/lib/mysql --log-error-verbosity=3 2>&1; exit 0"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/var/lib/mysql"},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			}},
		},
	}

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return wsrepRecoverResult{}, fmt.Errorf("failed to create wsrep_recover pod %s: %w", helperName, err)
	}

	// Wait for completion
	var podOutput string
	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		p, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, helperName, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		phase := string(p.Status.Phase)
		if phase == "Succeeded" || phase == "Failed" {
			podOutput = b.getHelperPodOutput(ctx, helperName)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, helperName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			time.Sleep(2 * time.Second)
			break
		}
	}

	if podOutput == "" {
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, helperName, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
		return wsrepRecoverResult{}, fmt.Errorf("wsrep_recover pod %s produced no output or timed out", helperName)
	}

	common.DebugLog("wsrep_recover output for %s:\n%s", podName, podOutput)

	return parseWsrepRecoverOutput(podOutput)
}

// parseWsrepRecoverOutput extracts UUID, seqno, and lastCommitted from
// mariadbd --wsrep-recover output.
func parseWsrepRecoverOutput(output string) (wsrepRecoverResult, error) {
	result := wsrepRecoverResult{}

	posMatch := reRecoveredPos.FindStringSubmatch(output)
	if posMatch == nil {
		return result, fmt.Errorf("could not parse recovered position from wsrep_recover output")
	}

	result.UUID = posMatch[1]
	seqno, err := strconv.ParseInt(posMatch[2], 10, 64)
	if err != nil {
		return result, fmt.Errorf("could not parse seqno %q: %w", posMatch[2], err)
	}
	result.Seqno = seqno

	// Parse LastCommitted — default to Seqno if not found (means fully applied)
	commitMatch := reLastCommit.FindStringSubmatch(output)
	if commitMatch != nil {
		lc, err := strconv.ParseInt(commitMatch[1], 10, 64)
		if err == nil {
			result.LastCommitted = lc
		} else {
			result.LastCommitted = seqno
		}
	} else {
		result.LastCommitted = seqno
	}

	// Validate UUID
	if result.UUID == zeroUUID {
		return result, fmt.Errorf("recovered zero UUID — node never joined cluster")
	}

	// Validate seqno range
	if result.Seqno < 0 || result.Seqno > maxPhantomSeqno {
		return result, fmt.Errorf("recovered phantom seqno %d — corrupt gcache metadata", result.Seqno)
	}

	result.Valid = true
	return result, nil
}

// getHelperPodOutput fetches logs from a helper pod and returns them as a string.
func (b *galeraBootstrap) getHelperPodOutput(ctx context.Context, podName string) string {
	c := k8s.GetClients()
	cfg := b.p.Config()
	req := c.Clientset.CoreV1().Pods(cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	return string(data)
}

// ---------------------------------------------------------------------------
// K8s helper methods (duplicated from galera engine — bootstrap is independent
// of repair and needs its own copy of these operations)
// ---------------------------------------------------------------------------

// suspendCR patches the MariaDB CR to set spec.suspend=true.
func (b *galeraBootstrap) suspendCR(ctx context.Context) error {
	c := k8s.GetClients()
	cfg := b.p.Config()
	patch := `{"spec":{"suspend":true}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// resumeCR patches the MariaDB CR to set spec.suspend=false.
func (b *galeraBootstrap) resumeCR(ctx context.Context) error {
	c := k8s.GetClients()
	cfg := b.p.Config()
	patch := `{"spec":{"suspend":false}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// scaleStatefulSet scales the StatefulSet to the desired replica count.
func (b *galeraBootstrap) scaleStatefulSet(ctx context.Context, replicas int32) error {
	c := k8s.GetClients()
	cfg := b.p.Config()
	scale, err := c.Clientset.AppsV1().StatefulSets(cfg.Namespace).GetScale(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = c.Clientset.AppsV1().StatefulSets(cfg.Namespace).UpdateScale(
		ctx, cfg.ClusterName, scale, metav1.UpdateOptions{})
	return err
}

// runHelperPod creates a busybox pod that mounts a PVC and runs a script,
// waits for completion, fetches logs, and cleans up.
func (b *galeraBootstrap) runHelperPod(ctx context.Context, name, pvcName, mountPath, script, sa string) error {
	cfg := b.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	rootUser := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "heal-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: sa,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &rootUser,
			},
			Containers: []corev1.Container{{
				Name:    "healer",
				Image:   "docker.io/library/busybox:latest",
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: mountPath},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			}},
		},
	}

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create helper pod %s: %w", name, err)
	}

	// Wait for completion
	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		p, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		phase := string(p.Status.Phase)
		if phase == "Succeeded" {
			b.logHelperPodOutput(ctx, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			time.Sleep(2 * time.Second)
			return nil
		}
		if phase == "Failed" {
			b.logHelperPodOutput(ctx, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			return fmt.Errorf("helper pod %s failed", name)
		}
	}

	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	return fmt.Errorf("helper pod %s timed out", name)
}

// logHelperPodOutput fetches and displays logs from a helper pod.
func (b *galeraBootstrap) logHelperPodOutput(ctx context.Context, podName string) {
	c := k8s.GetClients()
	cfg := b.p.Config()
	req := c.Clientset.CoreV1().Pods(cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		common.DebugLog("Failed to get helper pod logs: %v", err)
		return
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	if len(data) > 0 {
		common.DebugLog("Helper pod output:\n%s", string(data))
	}
}

// deleteRecoveryPods removes stale mariadb-operator recovery pods.
func (b *galeraBootstrap) deleteRecoveryPods(ctx context.Context) {
	c := k8s.GetClients()
	cfg := b.p.Config()
	pods, err := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + cfg.ClusterName + ",k8s.mariadb.com/recovery=true",
	})
	if err != nil {
		return
	}
	for _, p := range pods.Items {
		_ = c.Clientset.CoreV1().Pods(cfg.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
	}
}

// waitForAllReady waits for all StatefulSet pods to become Running and Ready.
// Soft timeout of 15 minutes — continues to verify step if not all ready.
func (b *galeraBootstrap) waitForAllReady(ctx context.Context) {
	c := k8s.GetClients()
	cfg := b.p.Config()
	expected := int(b.p.Replicas())

	// 90 iterations × 10s = 15 minutes soft timeout
	for i := 0; i < 90; i++ {
		pods, err := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/instance=" + cfg.ClusterName,
		})
		if err == nil {
			ready := 0
			for _, p := range pods.Items {
				if p.Status.Phase == "Running" && len(p.Status.ContainerStatuses) > 0 && p.Status.ContainerStatuses[0].Ready {
					ready++
				}
			}
			if ready == expected {
				common.InfoLog("All %d pods are Running and Ready", expected)
				return
			}
			common.DebugLog("Ready: %d/%d", ready, expected)
		}
		time.Sleep(10 * time.Second)
	}
	common.WarnLog("Not all pods became ready within 15 minute timeout — continuing to verify step")
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T { return &v }

