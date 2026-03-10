package cnpg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// healConfig holds per-repair prerequisites discovered from the primary.
type healConfig struct {
	primaryIP      string
	postgresUID    string
	postgresGID    string
	imageName      string
	serviceAccount string
}

func (e *Engine) Repair(ctx context.Context) (*common.RepairResult, error) {
	start := time.Now()
	repairResult := &common.RepairResult{}

	// Phase 1: Full triage
	output.Section("Phase 1: Triage")
	result, err := e.Triage(ctx)
	if err != nil {
		return nil, fmt.Errorf("triage failed: %w", err)
	}

	// Phase 2: Prepare heal prerequisites
	primary := k8s.GetNestedString(e.cluster, "status", "currentPrimary")
	if primary == "" {
		return nil, fmt.Errorf("ABORT: No primary detected. Cannot heal replicas without a healthy primary")
	}

	// Verify primary is running and ready
	c := k8s.GetClients()
	primaryPod, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).Get(ctx, primary, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("ABORT: Primary pod %s not found: %w", primary, err)
	}
	if primaryPod.Status.Phase != "Running" || len(primaryPod.Status.ContainerStatuses) == 0 || !primaryPod.Status.ContainerStatuses[0].Ready {
		return nil, fmt.Errorf("ABORT: Primary %s is not running and ready. Fix primary first", primary)
	}

	primaryIP := primaryPod.Status.PodIP

	// Get postgres UID/GID from primary
	uid := "26"
	uidResult, uidErr := k8s.ExecCommand(ctx, primary, e.cfg.Namespace, "postgres", []string{"id", "-u", "postgres"})
	if uidErr == nil {
		uid = strings.TrimSpace(uidResult.Stdout)
	}
	gid := "26"
	gidResult, gidErr := k8s.ExecCommand(ctx, primary, e.cfg.Namespace, "postgres", []string{"id", "-g", "postgres"})
	if gidErr == nil {
		gid = strings.TrimSpace(gidResult.Stdout)
	}
	common.DebugLog("Postgres UID/GID: %s/%s", uid, gid)

	// Get image name from cluster spec
	imageName := k8s.GetNestedString(e.cluster, "spec", "imageName")

	// Phase 2.5: Pre-repair escrow backup (always type=backup, follows normal retention)
	if !e.cfg.NoEscrow {
		if e.cfg.BackupsPath == "" || e.cfg.ResticPassword == "" {
			return nil, fmt.Errorf("repair requires --backups-path and RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
		}
		ns := e.cfg.Namespace
		stdinFilename := fmt.Sprintf("%s/%s/%s", ns, e.cfg.ClusterName, DumpFilename)
		escrowResult, err := e.BackupDump(ctx, "backup", primary, stdinFilename, start, nil)
		if err != nil {
			return nil, fmt.Errorf("pre-repair backup failed: %w", err)
		}
		common.InfoLog("Pre-repair backup: %s", escrowResult.SnapshotID)
	} else {
		common.WarnLog("no_escrow=true — proceeding without pre-repair backup")
	}

	// Phase 2.6: Diverged per-instance backups (when split-brain detected)
	if !result.DataComparison.SafeToHeal && !e.cfg.NoEscrow {
		jobID := start.UTC().Format("20060102T150405Z")
		common.WarnLog("Split-brain detected — capturing per-instance diverged backups (job=%s)", jobID)
		ns := e.cfg.Namespace
		for _, a := range result.Assessments {
			if !a.IsRunning || !a.IsReady {
				common.WarnLog("Skipping diverged backup for %s (not running/ready)", a.Pod)
				continue
			}
			stdinFilename := fmt.Sprintf("%s/%s/%d-%s", ns, e.cfg.ClusterName, a.Instance, DumpFilename)
			extraTags := map[string]string{"job": jobID}
			divResult, err := e.BackupDump(ctx, "diverged", a.Pod, stdinFilename, start, extraTags)
			if err != nil {
				common.WarnLog("Failed diverged backup for %s: %v", a.Pod, err)
				continue
			}
			common.InfoLog("Diverged backup %s: %s", a.Pod, divResult.SnapshotID)
		}
	}

	// Phase 3: Branch on targeted vs untargeted
	hcfg := &healConfig{
		primaryIP:      primaryIP,
		postgresUID:    uid,
		postgresGID:    gid,
		imageName:      imageName,
		serviceAccount: primaryPod.Spec.ServiceAccountName,
	}

	if e.cfg.InstanceNumber != nil {
		err = e.repairTargeted(ctx, result, hcfg, repairResult)
	} else {
		err = e.repairUntargeted(ctx, result, hcfg, repairResult)
	}

	if err != nil {
		return nil, err
	}

	repairResult.Duration = time.Since(start)
	return repairResult, nil
}

func (e *Engine) repairTargeted(ctx context.Context, result *common.TriageResult, hcfg *healConfig, repairResult *common.RepairResult) error {
	targetPod := fmt.Sprintf("%s-%d", e.cfg.ClusterName, *e.cfg.InstanceNumber)
	primary := k8s.GetNestedString(e.cluster, "status", "currentPrimary")

	// Safety gate: target is primary → HARD STOP
	if targetPod == primary {
		return fmt.Errorf("ABORT: %s is the PRIMARY. Cannot heal primary. Use switchover first", targetPod)
	}

	// Find target assessment
	var targetAssessment *common.InstanceAssessment
	for i := range result.Assessments {
		if result.Assessments[i].Pod == targetPod {
			targetAssessment = &result.Assessments[i]
			break
		}
	}
	if targetAssessment == nil {
		return fmt.Errorf("ABORT: %s not found in instance assessments. Check cluster_name and instance_number", targetPod)
	}

	// Safety gate: split-brain → fail unless force
	if !result.DataComparison.SafeToHeal && !e.cfg.Force {
		return fmt.Errorf("ABORT: Split-brain detected. Healing %s may cause DATA LOSS. Re-run with --force to override", targetPod)
	}
	if !result.DataComparison.SafeToHeal && e.cfg.Force {
		common.WarnLog("force=true - proceeding despite split-brain detection. Data on %s will be DESTROYED", targetPod)
	}

	// Safety gate: target is healthy → skip unless force
	if !targetAssessment.NeedsHeal && !e.cfg.Force {
		output.Info("Instance %s is healthy and does not need healing. Nothing to do.", targetPod)
		repairResult.SkippedInstances = append(repairResult.SkippedInstances, targetPod)
		return nil
	}
	if !targetAssessment.NeedsHeal && e.cfg.Force {
		common.WarnLog("force=true - healing %s even though it appears healthy", targetPod)
	}

	// Verify PVC exists (CNPG PVC name = pod name)
	c := k8s.GetClients()
	_, err := c.Clientset.CoreV1().PersistentVolumeClaims(e.cfg.Namespace).Get(ctx, targetPod, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("PVC %s not found: %w", targetPod, err)
	}

	// Heal
	if err := e.healInstance(ctx, targetPod, targetPod, hcfg); err != nil {
		return err
	}
	repairResult.HealedInstances = append(repairResult.HealedInstances, targetPod)

	// Display final status
	e.displayFinalStatus(ctx)
	return nil
}

func (e *Engine) repairUntargeted(ctx context.Context, result *common.TriageResult, hcfg *healConfig, repairResult *common.RepairResult) error {
	// Safety gate: split-brain → HARD STOP (no override for untargeted)
	if !result.DataComparison.SafeToHeal {
		return fmt.Errorf("HARD STOP: Split-brain detected. Cannot auto-heal all replicas. " +
			"Admin must review triage output, then use targeted repair: --instance <N>")
	}

	// Compute heal targets
	var targets []common.InstanceAssessment
	for _, a := range result.Assessments {
		if a.NeedsHeal {
			targets = append(targets, a)
		} else {
			repairResult.SkippedInstances = append(repairResult.SkippedInstances, a.Pod)
		}
	}

	if len(targets) == 0 {
		output.Info("All replicas are healthy. Nothing to heal.")
		return nil
	}

	// Display plan
	output.Section("Repair Plan")
	for _, t := range targets {
		notes := "needs heal"
		if len(t.Notes) > 0 {
			notes = strings.Join(t.Notes, ", ")
		}
		output.Bullet(0, "%s (%s)", t.Pod, notes)
	}

	// Heal each sequentially
	for _, t := range targets {
		pvc := t.Pod // CNPG PVC name = pod name
		if err := e.healInstance(ctx, t.Pod, pvc, hcfg); err != nil {
			return fmt.Errorf("heal failed for %s: %w", t.Pod, err)
		}
		repairResult.HealedInstances = append(repairResult.HealedInstances, t.Pod)
	}

	// Post-repair stabilization
	output.Section("Post-Repair Stabilization")
	common.InfoLog("Waiting 30s for CNPG operator to reconcile...")
	time.Sleep(30 * time.Second)

	e.waitForAllReady(ctx)

	// Re-triage: re-fetch cluster state first
	output.Section("Post-Repair Re-Triage")
	obj, err := k8s.GetClients().Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Get(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err == nil {
		e.cluster = obj
		e.clusterSpec = k8s.GetNestedMap(obj, "spec")
		e.clusterStatus = k8s.GetNestedMap(obj, "status")
		e.clusterAnnotations = k8s.GetNestedMap(obj, "metadata", "annotations")
		e.fencedInstances = parseFencedInstances(e.clusterAnnotations)
	}
	postTriage, _ := e.Triage(ctx)
	repairResult.PostTriageResult = postTriage

	return nil
}

func (e *Engine) waitForAllReady(ctx context.Context) {
	c := k8s.GetClients()
	expected := int(e.instances)

	for i := 0; i < 30; i++ {
		pods, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "cnpg.io/cluster=" + e.cfg.ClusterName,
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

func (e *Engine) displayFinalStatus(ctx context.Context) {
	c := k8s.GetClients()
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Get(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return
	}

	phase := k8s.GetNestedString(obj, "status", "phase")
	ready := k8s.GetNestedInt64(obj, "status", "readyInstances")
	instances := k8s.GetNestedInt64(obj, "spec", "instances")

	output.Section("Final Status")
	output.Field("Cluster", phase)
	output.Field("Ready", fmt.Sprintf("%d/%d", ready, instances))

	pods, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cnpg.io/cluster=" + e.cfg.ClusterName,
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
