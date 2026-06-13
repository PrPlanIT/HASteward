package repair

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// breakerReserveBytes is headroom that must remain free in the escrow store
// beyond the capture estimate, so an escrow cannot fill the repository to the brim.
const breakerReserveBytes int64 = 1 << 30 // 1 GiB

// PreAssess is repair Phase 0: the CNPG deadlock breaker. It is INERT unless
// --unwedge (cfg.DeadlockBreaker) is set AND triage reports a breakable deadlock
// (a disk-full disposable replica freezing the cluster, with unambiguous
// authority). When it fires it: selects a fail-closed escrow provider and proves
// there is space; captures + verifies a reversible escrow of the recovery set;
// re-triages and binds a RecoveryProof to that snapshot (so nothing drifted);
// PERSISTS the proof before any mutation; then clears each disposable instance's
// datadir offline — each gated immediately by proof.AuthorizesClear so the
// authority can never be cleared; and waits for CNPG to unfreeze.
//
// Any refusal returns an error and mutates nothing past escrow. On a clear/wait
// failure the verified escrow is RETAINED (logged) so rollback stays possible.
// Returns nil when inert; otherwise the post-break triage.
func (r *cnpgRepair) PreAssess(ctx context.Context) (*model.TriageResult, error) {
	cfg := r.p.Config()
	if !cfg.Unwedge {
		return nil, nil // inert — normal repair (which aborts on "no primary") proceeds unchanged
	}

	// 1. Raw triage (no primary prerequisite, unlike Assess).
	t, err := triage.Run(ctx, r.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("unwedge: triage failed: %w", err)
	}
	rec := t.Recovery
	if rec == nil || !rec.Blocked || rec.Reason != "disk_full_disposable_replica" {
		common.InfoLog("unwedge: no breakable deadlock detected — deferring to normal repair")
		return t, nil
	}
	if t.AuthorityStatus != "unambiguous" || rec.Authority == "" || len(rec.Disposable) == 0 {
		return nil, fmt.Errorf("unwedge REFUSED: deadlock present but authority is ambiguous or no disposable target — manual review required")
	}

	output.Section("Phase 0: Deadlock breaker (--unwedge)")
	output.Field("Authority", rec.Authority)
	output.Field("Disposable", strings.Join(rec.Disposable, ", "))
	output.Field("Recovery set", strings.Join(rec.RecoverySet, ", "))

	// 2. Select an escrow provider (fail-closed) and prove there is space BEFORE
	//    any capture — "requires X, only Y available", never a full repo mid-escrow.
	prov, err := escrow.Select(ctx, cfg, rec.RecoverySet)
	if err != nil {
		return nil, fmt.Errorf("unwedge REFUSED: %w", err)
	}
	used := usedBytesByPVC(t, rec.RecoverySet)
	est := prov.EstimateCaptureBytes(rec.RecoverySet, used)
	avail, aerr := prov.AvailableBytes()
	if aerr != nil {
		return nil, fmt.Errorf("unwedge REFUSED: cannot determine escrow free space: %w", aerr)
	}
	if est+breakerReserveBytes > avail {
		return nil, fmt.Errorf("unwedge REFUSED: escrow (%s) requires %s + %s reserve, only %s available in the escrow store",
			prov.Name(), output.FormatBytes(est), output.FormatBytes(breakerReserveBytes), output.FormatBytes(avail))
	}
	output.Field("Escrow", fmt.Sprintf("%s — ~%s needed, %s available", prov.Name(), output.FormatBytes(est), output.FormatBytes(avail)))

	// 3. Capture + verify the escrow (the rollback that authorizes the clear).
	refs, err := prov.Capture(ctx, rec.RecoverySet)
	if err != nil {
		return nil, fmt.Errorf("unwedge REFUSED: escrow capture failed: %w", err)
	}
	if err := prov.Verify(ctx, refs); err != nil {
		return nil, fmt.Errorf("unwedge REFUSED: escrow verification failed (rollback unproven): %w", err)
	}

	// 4. Re-triage at the destructive edge and bind the proof to THAT snapshot.
	confirm, err := triage.Run(ctx, r.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("unwedge REFUSED: re-triage failed: %w", err)
	}
	proof := RecoveryProof{
		AssessmentHash: hashAssessment(confirm),
		Authority:      rec.Authority,
		Disposable:     rec.Disposable,
		EscrowRefs:     refs,
		EscrowVerified: true,
	}
	if !proof.Valid(confirm) {
		return nil, fmt.Errorf("unwedge REFUSED: cluster state drifted after escrow (authority/disposability changed) — refusing to clear")
	}

	// 5. Persist the proof BEFORE any clear, so the audit of the most important
	//    action survives a crash mid-clear.
	if err := r.persistRecoveryProof(ctx, proof); err != nil {
		return nil, fmt.Errorf("unwedge REFUSED: cannot persist the decision record before clear: %w", err)
	}

	// 6. Build the clear config without a primary, then clear each disposable —
	//    each gated IMMEDIATELY by AuthorizesClear (never the authority).
	hcfg, err := r.buildBreakerConfig(ctx, confirm)
	if err != nil {
		return nil, fmt.Errorf("unwedge: %w", err)
	}
	for _, inst := range rec.Disposable {
		if !proof.AuthorizesClear(inst) {
			return nil, fmt.Errorf("unwedge ABORT: %s is not an authorized clear target (authority=%s) — refusing", inst, proof.Authority)
		}
		pvc := inst // CNPG pgdata PVC is named after its instance
		if err := r.clearDatadirOffline(ctx, inst, pvc, hcfg); err != nil {
			return nil, fmt.Errorf("unwedge: clearing %s failed; escrow %v RETAINED for rollback: %w", inst, escrowIDs(refs), err)
		}
	}

	// 7. Wait for CNPG to leave the disk-full freeze and the authority to be Ready.
	if err := r.waitUnfrozen(ctx, rec.Authority); err != nil {
		return nil, fmt.Errorf("unwedge: datadirs cleared but cluster did not recover; escrow %v RETAINED: %w", escrowIDs(refs), err)
	}

	output.Success("Deadlock broken: %d disposable datadir(s) cleared, cluster unfrozen, authority %s up", len(rec.Disposable), rec.Authority)
	// Escrow is intentionally NOT cleaned up here: the rollback window stays open
	// until the cluster is confirmed healthy (a later phase / the operator).
	return confirm, nil
}

// usedBytesByPVC pulls each recovery-set PVC's used bytes from triage's DiskStats,
// for the escrow space estimate. Missing/unknown disk → 0 (the estimate is a
// guard, not an accounting record).
func usedBytesByPVC(t *model.TriageResult, set []string) map[string]int64 {
	want := make(map[string]bool, len(set))
	for _, p := range set {
		want[p] = true
	}
	out := make(map[string]int64, len(set))
	for _, a := range t.Assessments {
		if want[a.Pod] && a.Disk != nil {
			out[a.Pod] = a.Disk.UsedBytes
		}
	}
	return out
}

// buildBreakerConfig assembles the clear pod's prerequisites WITHOUT a primary
// (the breaker runs while the cluster is frozen). CNPG runs postgres as uid/gid
// 26; the image comes from the cluster spec; the service account is inherited
// from an existing cluster pod (the down/crash-looping instances still carry it).
func (r *cnpgRepair) buildBreakerConfig(ctx context.Context, t *model.TriageResult) (*healConfig, error) {
	cfg := r.p.Config()
	c := k8s.GetClients()

	imageName := k8s.GetNestedString(r.p.Cluster(), "spec", "imageName")
	if imageName == "" {
		return nil, fmt.Errorf("cannot determine cluster imageName for the clear pod")
	}

	sa := "default"
	for _, a := range t.Assessments {
		if p, err := c.Clientset.CoreV1().Pods(cfg.Namespace).Get(ctx, a.Pod, metav1.GetOptions{}); err == nil && p.Spec.ServiceAccountName != "" {
			sa = p.Spec.ServiceAccountName
			break
		}
	}

	return &healConfig{postgresUID: "26", postgresGID: "26", imageName: imageName, serviceAccount: sa}, nil
}

// persistRecoveryProof writes the decision record to a ConfigMap BEFORE the clear,
// so months later an operator can answer "why did HASteward believe it was safe
// to clear this PVC?" without reconstructing from logs. Small by design.
func (r *cnpgRepair) persistRecoveryProof(ctx context.Context, proof RecoveryProof) error {
	cfg := r.p.Config()
	c := k8s.GetClients()

	var escrowList []string
	for _, ref := range proof.EscrowRefs {
		escrowList = append(escrowList, fmt.Sprintf("%s:%s(pvc=%s,verified=%t)", ref.Provider, ref.ID, ref.PVC, ref.Verified))
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("hasteward-unwedge-%s-%d", cfg.ClusterName, time.Now().Unix()),
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "hasteward",
				"hasteward":                    "unwedge-decision",
				"hasteward.cluster":            cfg.ClusterName,
			},
		},
		Data: map[string]string{
			"operation":      "repair-unwedge",
			"timestamp":      time.Now().UTC().Format(time.RFC3339),
			"authority":      proof.Authority,
			"disposable":     strings.Join(proof.Disposable, ","),
			"assessmentHash": proof.AssessmentHash,
			"escrow":         strings.Join(escrowList, "; "),
		},
	}
	_, err := c.Clientset.CoreV1().ConfigMaps(cfg.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	return err
}

// waitUnfrozen polls until CNPG leaves the disk-full freeze AND the authority pod
// is Running+Ready — confirming the clear actually un-stuck the cluster.
func (r *cnpgRepair) waitUnfrozen(ctx context.Context, authority string) error {
	cfg := r.p.Config()
	c := k8s.GetClients()
	for i := 0; i < 60; i++ { // up to 10 min
		time.Sleep(10 * time.Second)

		phase := ""
		if cl, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(ctx, cfg.ClusterName, metav1.GetOptions{}); err == nil {
			phase = k8s.GetNestedString(cl, "status", "phase")
		}
		frozen := strings.Contains(strings.ToLower(phase), "disk space")

		ready := false
		if ap, err := c.Clientset.CoreV1().Pods(cfg.Namespace).Get(ctx, authority, metav1.GetOptions{}); err == nil {
			ready = ap.Status.Phase == corev1.PodRunning &&
				len(ap.Status.ContainerStatuses) > 0 && ap.Status.ContainerStatuses[0].Ready
		}

		if !frozen && ready {
			return nil
		}
		common.InfoLog("Waiting for unfreeze: phase=%q authorityReady=%t (%ds)", phase, ready, (i+1)*10)
	}
	return fmt.Errorf("cluster still frozen or authority %s not Ready after 10m", authority)
}

// escrowIDs lists the backend handles of the escrow refs, for retention-on-failure
// log lines.
func escrowIDs(refs []escrow.EscrowRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids
}
