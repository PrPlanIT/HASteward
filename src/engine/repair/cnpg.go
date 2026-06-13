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
	p       *provider.CNPGProvider
	triager triage.Triager
	backuper backup.Backer

	// Populated during Assess, used by later phases.
	hcfg *healConfig
}

func (r *cnpgRepair) Name() string { return "cnpg" }

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
	if primaryPod.Status.Phase != "Running" || len(primaryPod.Status.ContainerStatuses) == 0 || !primaryPod.Status.ContainerStatuses[0].Ready {
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

// Escrow performs the pre-repair escrow backup and diverged per-instance backups.
func (r *cnpgRepair) Escrow(ctx context.Context, result *model.TriageResult) error {
	cfg := r.p.Config()
	start := time.Now()

	if !cfg.NoEscrow {
		if cfg.BackupsPath == "" || cfg.ResticPassword == "" {
			return fmt.Errorf("repair requires --backups-path and RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
		}

		primary := k8s.GetNestedString(r.p.Cluster(), "status", "currentPrimary")
		ns := cfg.Namespace
		stdinFilename := fmt.Sprintf("%s/%s/%s", ns, cfg.ClusterName, cnpgDumpFilename)
		escrowResult, err := r.backuper.BackupDump(ctx, "backup", primary, stdinFilename, start, nil)
		if err != nil {
			return fmt.Errorf("pre-repair backup failed: %w", err)
		}
		common.InfoLog("Pre-repair backup: %s", escrowResult.SnapshotID)
	} else {
		common.WarnLog("no_escrow=true — proceeding without pre-repair backup")
	}

	// Diverged per-instance backups (when split-brain detected)
	if !result.DataComparison.SafeToHeal && !cfg.NoEscrow {
		jobID := start.UTC().Format("20060102T150405Z")
		common.WarnLog("Split-brain detected — capturing per-instance diverged backups (job=%s)", jobID)
		ns := cfg.Namespace
		for _, a := range result.Assessments {
			if !a.IsRunning || !a.IsReady {
				common.WarnLog("Skipping diverged backup for %s (not running/ready)", a.Pod)
				continue
			}
			stdinFilename := fmt.Sprintf("%s/%s/%d-%s", ns, cfg.ClusterName, a.Instance, cnpgDumpFilename)
			extraTags := map[string]string{"job": jobID}
			divResult, err := r.backuper.BackupDump(ctx, "diverged", a.Pod, stdinFilename, start, extraTags)
			if err != nil {
				common.WarnLog("Failed diverged backup for %s: %v", a.Pod, err)
				continue
			}
			common.InfoLog("Diverged backup %s: %s", a.Pod, divResult.SnapshotID)
		}
	}

	return nil
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
	}}, nil
}

func (r *cnpgRepair) planUntargeted(ctx context.Context, result *model.TriageResult) ([]HealTarget, error) {
	// Safety gate: split-brain -> HARD STOP (no override for untargeted)
	if !result.DataComparison.SafeToHeal {
		return nil, fmt.Errorf("HARD STOP: Split-brain detected. Cannot auto-heal all replicas. " +
			"Admin must review triage output, then use targeted repair: --instance <N>")
	}

	var targets []HealTarget
	for _, a := range result.Assessments {
		if a.NeedsHeal {
			reason := "needs heal"
			if len(a.Notes) > 0 {
				reason = strings.Join(a.Notes, ", ")
			}
			targets = append(targets, HealTarget{
				Pod:         a.Pod,
				InstanceNum: a.Instance,
				Reason:      reason,
			})
		}
	}

	if len(targets) == 0 {
		output.Info("All replicas are healthy. Nothing to heal.")
		return nil, nil
	}

	// Display plan
	output.Section("Repair Plan")
	for _, t := range targets {
		output.Bullet(0, "%s (%s)", t.Pod, t.Reason)
	}

	return targets, nil
}

// Heal heals a single CNPG replica via fence/clear/basebackup/unfence.
func (r *cnpgRepair) Heal(ctx context.Context, target HealTarget) error {
	pvc := target.Pod // CNPG PVC name = pod name
	return r.healInstance(ctx, target.Pod, pvc, r.hcfg)
}

// Stabilize waits for the operator to reconcile and all pods to become ready.
func (r *cnpgRepair) Stabilize(ctx context.Context) error {
	output.Section("Post-Repair Stabilization")
	common.InfoLog("Waiting 30s for CNPG operator to reconcile...")
	time.Sleep(30 * time.Second)
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
	output.Bullet(0, "4. Run pg_basebackup from primary (%s)", hcfg.primaryIP)
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

echo "=== pg_basebackup complete! ==="`, hcfg.primaryIP)

	healPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      healPodName,
			Namespace: ns,
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
		time.Sleep(10 * time.Second)
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if podErr == nil && pod.Status.Phase == "Running" &&
			len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
			output.Success("Replica %s has been healed!", targetPod)
			return nil
		}
	}

	common.WarnLog("%s did not become ready within timeout. CNPG may still be reconciling.", targetPod)
	return nil
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
		LabelSelector: "cnpg.io/cluster=" + cfg.ClusterName,
	})
	if err == nil {
		for _, p := range pods.Items {
			podReady := false
			if len(p.Status.ContainerStatuses) > 0 {
				podReady = p.Status.ContainerStatuses[0].Ready
			}
			output.Bullet(0, "%s: %s ready=%v", p.Name, p.Status.Phase, podReady)
		}
	}
}

// waitForAllReady polls until all expected instances are Running and Ready.
func (r *cnpgRepair) waitForAllReady(ctx context.Context) {
	cfg := r.p.Config()
	c := k8s.GetClients()
	expected := int(r.p.Instances())

	for i := 0; i < 30; i++ {
		pods, err := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "cnpg.io/cluster=" + cfg.ClusterName,
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
	common.WarnLog("Not all pods became ready within timeout")
}
