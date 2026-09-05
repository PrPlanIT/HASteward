package repair

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/backup"
	"github.com/PrPlanIT/HASteward/src/engine/cnpgjob"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const cnpgDumpFilename = "pgdumpall.sql"

func init() {
	Register("cnpg", func(p provider.EngineProvider) (Repairer, error) {
		cp, ok := p.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("cnpg repair: expected *provider.CNPGProvider, got %T", p)
		}
		t, err := triage.Get(p)
		if err != nil {
			return nil, fmt.Errorf("cnpg repair: triage init: %w", err)
		}
		b, err := backup.Get(p)
		if err != nil {
			return nil, fmt.Errorf("cnpg repair: backup init: %w", err)
		}
		return &cnpgRepair{p: cp, triager: t, backuper: b}, nil
	})
}

// healConfig holds per-repair prerequisites discovered from the primary.
type healConfig struct {
	primaryIP      string
	postgresUID    string
	postgresGID    string
	imageName      string
	serviceAccount string
}

// cnpgRepair implements Repairer for CloudNativePG PostgreSQL clusters.
type cnpgRepair struct {
	p        *provider.CNPGProvider
	triager  triage.Triager
	backuper backup.Backer

	// Populated during Assess, used by later phases.
	hcfg *healConfig
}

func (r *cnpgRepair) Name() string { return "cnpg" }

// DryRun reports whether this is a preview run (--dry-run).
func (r *cnpgRepair) DryRun() bool { return r.p.Config().DryRun }

// OperationLock takes the exclusive cluster lease for the duration of the repair.
func (r *cnpgRepair) OperationLock(ctx context.Context) (func(), error) {
	cfg := r.p.Config()
	return cnpgjob.AcquireClusterLock(ctx, cfg.Namespace, cfg.ClusterName, "repair")
}

// Assess runs a full triage of the CNPG cluster and discovers heal prerequisites.
func (r *cnpgRepair) Assess(ctx context.Context) (*model.TriageResult, error) {
	output.Section("Phase 1: Triage")
	result, err := triage.Run(ctx, r.triager, engine.NopSink{})
	if err != nil {
		return nil, err
	}

	// Discover heal prerequisites from the primary
	primary := k8s.GetNestedString(r.p.Cluster(), "status", "currentPrimary")
	if primary == "" {
		return nil, fmt.Errorf("ABORT: No primary detected. Cannot heal replicas without a healthy primary")
	}

	c := k8s.GetClients()
	primaryPod, err := c.Clientset.CoreV1().Pods(r.p.Config().Namespace).Get(ctx, primary, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("ABORT: Primary pod %s not found: %w", primary, err)
	}
	if !k8s.PodReady(*primaryPod, "postgres") {
		return nil, fmt.Errorf("ABORT: Primary %s is not running and ready. Fix primary first", primary)
	}

	uid := "26"
	uidResult, uidErr := k8s.ExecCommand(ctx, primary, r.p.Config().Namespace, "postgres", []string{"id", "-u", "postgres"})
	if uidErr == nil {
		uid = strings.TrimSpace(uidResult.Stdout)
	}
	gid := "26"
	gidResult, gidErr := k8s.ExecCommand(ctx, primary, r.p.Config().Namespace, "postgres", []string{"id", "-g", "postgres"})
	if gidErr == nil {
		gid = strings.TrimSpace(gidResult.Stdout)
	}
	common.DebugLog("Postgres UID/GID: %s/%s", uid, gid)

	imageName := k8s.GetNestedString(r.p.Cluster(), "spec", "imageName")

	r.hcfg = &healConfig{
		primaryIP:      primaryPod.Status.PodIP,
		postgresUID:    uid,
		postgresGID:    gid,
		imageName:      imageName,
		serviceAccount: primaryPod.Spec.ServiceAccountName,
	}

	return result, nil
}

// SafetyGate verifies the primary is running and ready (already done in Assess).
func (r *cnpgRepair) SafetyGate(ctx context.Context, result *model.TriageResult) error {
	// Primary validation already performed in Assess. Nothing additional needed.
	return nil
}

// Cleanup is a no-op for CNPG: it never suspends the operator CR for the run's
// duration, and the cluster-scoped reconciliation switch is released by
// OperationLock's returned func. Present to satisfy the Repairer contract.
func (r *cnpgRepair) Cleanup(ctx context.Context) {}

// Escrow performs the pre-repair escrow backup and diverged per-instance backups.
func (r *cnpgRepair) Escrow(ctx context.Context, result *model.TriageResult) error {
	primary := k8s.GetNestedString(r.p.Cluster(), "status", "currentPrimary")
	return runEscrow(ctx, r.p.Config(), r.backuper, result, primary, cnpgDumpFilename)
}

// PlanTargets determines which instances need healing.
func (r *cnpgRepair) PlanTargets(ctx context.Context, result *model.TriageResult) ([]HealTarget, error) {
	cfg := r.p.Config()

	if cfg.InstanceNumber != nil {
		return r.planTargeted(ctx, result)
	}
	return r.planUntargeted(ctx, result)
}

func (r *cnpgRepair) planTargeted(ctx context.Context, result *model.TriageResult) ([]HealTarget, error) {
	cfg := r.p.Config()
	targetPod := fmt.Sprintf("%s-%d", cfg.ClusterName, *cfg.InstanceNumber)
	primary := k8s.GetNestedString(r.p.Cluster(), "status", "currentPrimary")

	// Safety gate: target is primary -> HARD STOP
	if targetPod == primary {
		return nil, fmt.Errorf("ABORT: %s is the PRIMARY. Cannot heal primary. Use switchover first", targetPod)
	}

	// Fail-closed AUTHORITY guard (P3.2). Healing fences the target and rm -rf's its
	// pgdata, then re-clones from the PRIMARY. When triage names this target the data
	// AUTHORITY while the heal is unsafe — the winning lineage is on a replica, not the
	// primary (leader_not_primary), or a contested/undeterminable lineage — cloning the
	// primary over it paper-shreds the newest data. This is the exact "node nuked while
	// holding the most valid data" failure the authority determination exists to
	// prevent, so --force MUST NOT reach it: --force may override "looks healthy enough",
	// never "destroy the authority". Recovery is to rebuild the cluster AROUND the
	// authority (see the recovery plan in `hasteward triage`), not to heal it away.
	if !result.DataComparison.SafeToHeal && result.DataComparison.MostAdvanced != "" &&
		targetPod == result.DataComparison.MostAdvanced {
		return nil, fmt.Errorf("REFUSING to heal %s: triage identifies it as the DATA AUTHORITY (authority=%s), "+
			"not a disposable replica. Healing would rm -rf its pgdata and re-clone from the primary, DESTROYING "+
			"the newest data — --force cannot override this. To recover, rebuild the cluster AROUND %s "+
			"(escrow → relieve → promote the authority → heal the rest); run `hasteward triage -e cnpg -c %s -n %s` "+
			"for the ordered recovery plan", targetPod, result.DataComparison.Authority, targetPod, cfg.ClusterName, cfg.Namespace)
	}

	// Find target assessment
	var targetAssessment *model.InstanceAssessment
	for i := range result.Assessments {
		if result.Assessments[i].Pod == targetPod {
			targetAssessment = &result.Assessments[i]
			break
		}
	}
	if targetAssessment == nil {
		return nil, fmt.Errorf("ABORT: %s not found in instance assessments. Check cluster_name and instance_number", targetPod)
	}

	// Restart-viable target: data is intact and the WAL is retained, so the fix
	// is a non-destructive pod restart (CNPG recreates against the same PVC and
	// it streams back) — NOT a re-clone. It bypasses the reseed safety gates
	// below, which exist to guard the rm-rf/basebackup that restart never does.
	// --force opts INTO the destructive reseed instead (operator override).
	if targetAssessment.Remediation == "restart" && !cfg.Force {
		reason := "restart (data intact, WAL retained)"
		if len(targetAssessment.Notes) > 0 {
			reason = strings.Join(targetAssessment.Notes, ", ")
		}
		return []HealTarget{{Pod: targetPod, InstanceNum: *cfg.InstanceNumber, Reason: reason, Remediation: "restart"}}, nil
	}

	// Safety gate: split-brain -> fail unless force
	if !result.DataComparison.SafeToHeal && !cfg.Force {
		return nil, fmt.Errorf("ABORT: Split-brain detected. Healing %s may cause DATA LOSS. Re-run with --force to override", targetPod)
	}
	if !result.DataComparison.SafeToHeal && cfg.Force {
		common.WarnLog("force=true - proceeding despite split-brain detection. Data on %s will be DESTROYED", targetPod)
	}

	// Safety gate: target is healthy -> skip unless force
	if !targetAssessment.NeedsHeal && !cfg.Force {
		output.Info("Instance %s is healthy and does not need healing. Nothing to do.", targetPod)
		return nil, nil
	}
	if !targetAssessment.NeedsHeal && cfg.Force {
		common.WarnLog("force=true - healing %s even though it appears healthy", targetPod)
	}

	// Verify PVC exists (CNPG PVC name = pod name)
	c := k8s.GetClients()
	_, err := c.Clientset.CoreV1().PersistentVolumeClaims(cfg.Namespace).Get(ctx, targetPod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("PVC %s not found: %w", targetPod, err)
	}

	reason := "needs heal"
	if len(targetAssessment.Notes) > 0 {
		reason = strings.Join(targetAssessment.Notes, ", ")
	}
	return []HealTarget{{
		Pod:         targetPod,
		InstanceNum: *cfg.InstanceNumber,
		Reason:      reason,
		Remediation: "reseed",
	}}, nil
}

func (r *cnpgRepair) planUntargeted(ctx context.Context, result *model.TriageResult) ([]HealTarget, error) {
	return buildUntargetedPlan(result, "replicas")
}

// Heal applies a target's remediation: a non-destructive restart when the data
// is intact and the WAL retained, otherwise the fence/clear/basebackup/unfence
// re-clone.
func (r *cnpgRepair) Heal(ctx context.Context, target HealTarget) error {
	if target.Remediation == "restart" {
		return r.restartInstance(ctx, target.Pod)
	}
	pvc := target.Pod // CNPG PVC name = pod name
	return r.healInstance(ctx, target.Pod, pvc, r.hcfg)
}

// restartInstance is the non-destructive remediation: delete the pod and let
// CNPG recreate it against its EXISTING PVC. No fence, no rm-rf, no basebackup —
// the instance streams back from the WAL the primary still retains. The
// least-destructive fix, chosen when triage proves the data intact.
func (r *cnpgRepair) restartInstance(ctx context.Context, pod string) error {
	cfg := r.p.Config()
	output.Section("Restart " + pod + " — data intact, no re-clone")
	output.Bullet(0, "Clear any stale primary_conninfo shadow so CNPG owns replication")
	output.Bullet(0, "Delete pod; CNPG recreates it against the same PVC")
	output.Bullet(0, "Instance resumes streaming from the primary's retained WAL")
	if r.DryRun() {
		output.Info("[dry-run] would strip an auto.conf primary_conninfo shadow on %s, delete the pod, and wait for Ready", pod)
		return nil
	}
	// A restart only restores streaming if CNPG's override.conf actually governs
	// replication. A primary_conninfo left in postgresql.auto.conf by an EARLIER
	// pg_basebackup -R (a raw pod IP + throwaway /tmp/certs paths) is read LAST and
	// shadows override.conf, so the walreceiver loops FATAL and the pod comes up
	// Ready-but-not-streaming. The reseed path already strips this; the restart path
	// must too, or "restart" silently never restores streaming. Best-effort — if the
	// exec can't reach a crash-looping container, VerifyRecovery still fails the run
	// loud rather than reporting a false green.
	r.clearConninfoShadow(ctx, pod)
	c := k8s.GetClients()
	if err := c.Clientset.CoreV1().Pods(cfg.Namespace).Delete(ctx, pod, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod %s for restart: %w", pod, err)
	}
	common.InfoLog("Deleted %s; waiting for the cluster to become Ready", pod)
	r.waitForAllReady(ctx)
	return nil
}

// clearConninfoShadow removes any primary_conninfo from the instance's
// postgresql.auto.conf so CNPG's override.conf governs replication. Idempotent
// and best-effort: a crash-looping pod may be un-execable, in which case the
// post-repair VerifyRecovery is the backstop that fails the run loud. Mirrors the
// strip the basebackup/reseed heal already performs (healInstance), so BOTH heal
// paths leave the datadir in a CNPG-consistent, streamable state.
func (r *cnpgRepair) clearConninfoShadow(ctx context.Context, pod string) {
	cfg := r.p.Config()
	const autoConf = "/var/lib/postgresql/data/pgdata/postgresql.auto.conf"
	res, err := k8s.ExecCommand(ctx, pod, cfg.Namespace, "postgres", []string{
		"sh", "-c", fmt.Sprintf(
			`f=%s; if [ -f "$f" ] && grep -qiE '^[[:space:]]*primary_conninfo[[:space:]]*=' "$f"; then `+
				`sed -i -E '/^[[:space:]]*primary_conninfo[[:space:]]*=/d' "$f" && echo stripped; else echo clean; fi`,
			autoConf),
	})
	if err != nil {
		common.WarnLog("Could not clear a possible primary_conninfo shadow on %s (%v); "+
			"VerifyRecovery will confirm streaming after restart", pod, err)
		return
	}
	if strings.TrimSpace(res.Stdout) == "stripped" {
		common.InfoLog("Stripped a stale primary_conninfo from %s auto.conf; CNPG now owns the conninfo", pod)
	}
}

// Stabilize waits for the operator to reconcile and all pods to become ready.
func (r *cnpgRepair) Stabilize(ctx context.Context) error {
	output.Section("Post-Repair Stabilization")
	common.InfoLog("Waiting 30s for CNPG operator to reconcile...")
	common.Sleep(30 * time.Second)
	r.waitForAllReady(ctx)
	return nil
}

// Reassess re-fetches the CNPG cluster state and runs triage again.
func (r *cnpgRepair) Reassess(ctx context.Context) (*model.TriageResult, error) {
	output.Section("Post-Repair Re-Triage")
	cfg := r.p.Config()
	obj, err := k8s.GetClients().Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err == nil {
		r.p.SetCluster(obj)
	}
	return triage.Run(ctx, r.triager, engine.NopSink{})
}

// ---------------------------------------------------------------------------
// Private heal methods (from cnpg/heal.go)
// ---------------------------------------------------------------------------

// healInstance heals a single CNPG replica via fence/clear/basebackup/unfence.
func (r *cnpgRepair) healInstance(ctx context.Context, targetPod, targetPVC string, hcfg *healConfig) error {
	cfg := r.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	// #22 — fail-closed primary guard. Healing fences the instance and rm -rf's its
	// pgdata; doing that to the CURRENT PRIMARY is a self-inflicted outage and data
	// loss. Re-fetch live (not the cached CR) so a failover between assess and heal
	// can't slip a newly-promoted primary past a stale check. If we can't verify the
	// primary, refuse rather than gamble on a destructive rebuild.
	guardObj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("refusing to heal %s: cannot verify the current primary before a destructive rebuild: %w", targetPod, err)
	}
	primary := k8s.GetNestedString(guardObj, "status", "currentPrimary")
	if targetPod == primary {
		return fmt.Errorf("REFUSING to heal %s: it is the current primary — fencing and rebuilding the primary's datadir would cause an outage and data loss; fail over (switchover) to a replica first, then re-triage", targetPod)
	}

	// #24 — re-resolve the basebackup source live. hcfg.primaryIP was captured during
	// Assess; if CNPG failed the primary over since, basebackup would clone from a stale
	// node. Prefer the CURRENT primary's live PodIP (from the same fresh fetch that guards
	// above); fall back to the captured value if the live lookup hiccups (no regression).
	primaryIP := hcfg.primaryIP
	if primary != "" {
		if pp, perr := c.Clientset.CoreV1().Pods(ns).Get(ctx, primary, metav1.GetOptions{}); perr == nil && pp.Status.PodIP != "" {
			primaryIP = pp.Status.PodIP
		}
	}

	// Derive names
	parts := strings.Split(targetPod, "-")
	instanceSuffix := parts[len(parts)-1]
	healPodName := fmt.Sprintf("%s-heal-%s-%d", cfg.ClusterName, instanceSuffix, time.Now().Unix())
	caSecret := cfg.ClusterName + "-ca"
	replSecret := cfg.ClusterName + "-replication"

	output.Section("Healing " + targetPod)
	output.Bullet(0, "1. Fence instance (CNPG stops managing it)")
	output.Bullet(0, "2. Disable reconcile loop so the operator yields the PVC")
	output.Bullet(0, "3. Clear pgdata on PVC %s (PVC preserved)", targetPVC)
	output.Bullet(0, "4. Run pg_basebackup from primary (%s)", primaryIP)
	output.Bullet(0, "5. Remove fence + re-enable reconcile (CNPG takes over the replica)")

	uid, _ := strconv.ParseInt(hcfg.postgresUID, 10, 64)
	gid, _ := strconv.ParseInt(hcfg.postgresGID, 10, 64)

	healScript := fmt.Sprintf(`set -e
echo "=== Step 1: Clearing pgdata ==="
if [ -f /var/lib/postgresql/data/pgdata/PG_VERSION ]; then
  echo "WARNING: Found existing PG_VERSION file. Proceeding with clear..."
fi
rm -rf /var/lib/postgresql/data/pgdata/*
rm -rf /var/lib/postgresql/data/pgdata/.[!.]*
rm -rf /var/lib/postgresql/data/lost+found 2>/dev/null || true
echo "pgdata cleared."

echo "=== Step 2: Setting up TLS certificates ==="
mkdir -p /tmp/certs
cp /certs/ca/ca.crt /tmp/certs/
cp /certs/replication/tls.crt /tmp/certs/
cp /certs/replication/tls.key /tmp/certs/
chmod 600 /tmp/certs/tls.key
echo "TLS certs ready."

echo "=== Step 3: Running pg_basebackup ==="
pg_basebackup -h %s -p 5432 -U streaming_replica \
  -D /var/lib/postgresql/data/pgdata \
  -Fp -Xs -P -R \
  --checkpoint=fast \
  -d "sslmode=verify-ca sslcert=/tmp/certs/tls.crt sslkey=/tmp/certs/tls.key sslrootcert=/tmp/certs/ca.crt"

echo "=== pg_basebackup complete! ==="

echo "=== Finalizing: yield primary_conninfo to CNPG ==="
# pg_basebackup -R baked a primary_conninfo into postgresql.auto.conf pointing at THIS
# heal job's throwaway /tmp/certs paths. postgresql.auto.conf is read LAST, so it
# shadows CNPG's override.conf (which uses the durable /controller/certificates paths).
# Once this pod exits, /tmp/certs is gone and the reseeded replica's walreceiver loops
# FATAL on a missing CA — a replica that can never stream. Strip it; CNPG owns the
# conninfo. standby.signal (also written by -R) is retained so it starts as a standby.
if [ -f /var/lib/postgresql/data/pgdata/postgresql.auto.conf ]; then
  sed -i -E '/^[[:space:]]*primary_conninfo[[:space:]]*=/d' /var/lib/postgresql/data/pgdata/postgresql.auto.conf
  echo "primary_conninfo yielded to CNPG (standby.signal retained)."
fi`, primaryIP)

	healPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      healPodName,
			Namespace: ns,
			// cnpg.io/cluster admits the pod through CNPG replication
			// network contracts — policy-gated fleets allow 5432 only
			// between pods carrying the cluster label, and pg_basebackup
			// must reach the primary. The operator never reconciles this
			// pod: the reconcile loop is disabled for its whole lifetime.
			Labels: map[string]string{
				"cnpg.io/cluster": cfg.ClusterName,
				"hasteward":       "heal-helper",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: hcfg.serviceAccount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
				FSGroup:    &gid,
			},
			Containers: []corev1.Container{{
				Name:    "healer",
				Image:   hcfg.imageName,
				Command: []string{"sh", "-c", healScript},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
					{Name: "ca-certs", MountPath: "/certs/ca", ReadOnly: true},
					{Name: "replication-certs", MountPath: "/certs/replication", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "pgdata",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: targetPVC,
						},
					},
				},
				{
					Name: "ca-certs",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: caSecret,
							Items: []corev1.KeyToPath{
								{Key: "ca.crt", Path: "ca.crt"},
							},
						},
					},
				},
				{
					Name: "replication-certs",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: replSecret,
							Items: []corev1.KeyToPath{
								{Key: "tls.crt", Path: "tls.crt"},
								{Key: "tls.key", Path: "tls.key"},
							},
						},
					},
				},
			},
		},
	}
	// Runs as the postgres uid (non-root); pg_basebackup is a DB engine that also writes
	// throwaway certs under /tmp, so its rootfs stays writable.
	k8s.ApplyHelperHardening(healPod, k8s.HelperHardening{WritableRootFS: true})

	if err := cnpgjob.Run(ctx, cnpgjob.OfflinePVCJob{
		Namespace:          ns,
		ClusterName:        cfg.ClusterName,
		TargetPod:          targetPod,
		TargetPVC:          targetPVC,
		HelperPod:          healPod,
		HelperPodName:      healPodName,
		Label:              "heal",
		DeleteTimeoutSec:   cfg.DeleteTimeout,
		CompleteTimeoutSec: cfg.HealTimeout,
	}); err != nil {
		return err
	}

	// Wait for the operator to recreate + start the healed replica on its PVC.
	common.InfoLog("Waiting for %s to come back online", targetPod)
	for i := 0; i < 30; i++ {
		common.Sleep(10 * time.Second)
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if podErr == nil && k8s.PodReady(*pod, "postgres") {
			output.Success("Replica %s has been healed!", targetPod)
			return nil
		}
	}

	// The replica never came back Ready — it has NOT rejoined. Returning nil here
	// would let the orchestrator record it as healed; report the failure instead so
	// the result is honest (mirrors the galera #7 fix).
	return fmt.Errorf("%s did not become Ready after heal — it has not rejoined the cluster; "+
		"check the pod's logs (a large pg_basebackup may still be running) and re-triage", targetPod)
}

// displayFinalStatus shows the current cluster state after healing.
func (r *cnpgRepair) displayFinalStatus(ctx context.Context) {
	cfg := r.p.Config()
	c := k8s.GetClients()
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return
	}

	phase := k8s.GetNestedString(obj, "status", "phase")
	ready := k8s.GetNestedInt64(obj, "status", "readyInstances")
	instances := k8s.GetNestedInt64(obj, "spec", "instances")

	output.Section("Final Status")
	output.Field("Cluster", phase)
	output.Field("Ready", fmt.Sprintf("%d/%d", ready, instances))

	pods, err := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: r.p.PodSelector(),
	})
	if err == nil {
		for _, p := range pods.Items {
			podReady := k8s.ContainerReadyByName(p, "postgres")
			output.Bullet(0, "%s: %s ready=%v", p.Name, p.Status.Phase, podReady)
		}
	}
}

// waitForAllReady polls until all expected instances are Running and Ready.
func (r *cnpgRepair) waitForAllReady(ctx context.Context) {
	cfg := r.p.Config()
	expected := int(r.p.Instances())
	if k8s.WaitAllReady(ctx, cfg.Namespace, r.p.PodSelector(), expected, 30, 10, "postgres") {
		common.InfoLog("All %d pods are Running and Ready", expected)
		return
	}
	common.WarnLog("Not all pods became ready within timeout")
}

// VerifyRecovery confirms each healed replica is actually STREAMING from the
// primary (present in pg_stat_replication with state='streaming') — not merely
// Ready. This catches the stale-primary_conninfo shadow: a reseeded or restarted
// replica whose postmaster is up and Ready but whose walreceiver never connected,
// which CNPG's phase/readyInstances happily reports as healthy.
func (r *cnpgRepair) VerifyRecovery(ctx context.Context, healed []string) error {
	if len(healed) == 0 {
		return nil
	}
	cfg := r.p.Config()
	ns := cfg.Namespace
	// Resolve the primary LIVE — a switchover during heal may have moved it.
	obj, err := k8s.GetClients().Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot resolve the current primary to verify streaming: %w", err)
	}
	primary := k8s.GetNestedString(obj, "status", "currentPrimary")
	if primary == "" {
		return fmt.Errorf("cluster reports no currentPrimary; cannot verify streaming")
	}
	streaming, err := r.streamingStandbys(ctx, primary, ns)
	if err != nil {
		return fmt.Errorf("querying pg_stat_replication on primary %s: %w", primary, err)
	}
	var stranded []string
	for _, pod := range healed {
		if pod == primary {
			continue // a healed instance promoted to primary has no upstream to stream from
		}
		if !streaming[pod] {
			stranded = append(stranded, pod)
		}
	}
	if len(stranded) > 0 {
		return fmt.Errorf("instance(s) Ready but NOT streaming from primary %s: %s — the walreceiver never "+
			"connected (check postgresql.auto.conf for a stale primary_conninfo shadowing CNPG's override.conf); "+
			"the cluster is degraded despite reporting N/N Ready", primary, strings.Join(stranded, ", "))
	}
	common.InfoLog("Verified: all healed instance(s) are streaming from primary %s", primary)
	return nil
}

// streamingStandbys returns the set of application_names currently streaming from
// the primary. application_name == the CNPG instance (pod) name, so callers test
// membership by pod name. Reuses the same pg_stat_replication read triage performs.
func (r *cnpgRepair) streamingStandbys(ctx context.Context, primary, ns string) (map[string]bool, error) {
	res, err := k8s.ExecCommand(ctx, primary, ns, "postgres", []string{
		"psql", "-U", "postgres", "-d", "postgres", "-tAF", "|", "-c",
		"SELECT application_name FROM pg_stat_replication WHERE state = 'streaming'",
	})
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			set[name] = true
		}
	}
	return set, nil
}
