package galera

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Engine) Repair(ctx context.Context) (*model.RepairResult, error) {
	start := time.Now()
	repairResult := &model.RepairResult{
		Engine:  e.Name(),
		Cluster: model.ObjectRef{Namespace: e.cfg.Namespace, Name: e.cfg.ClusterName},
	}

	// Phase 1: Full triage
	output.Section("Phase 1: Triage")
	result, err := e.Triage(ctx)
	if err != nil {
		return nil, fmt.Errorf("triage failed: %w", err)
	}

	// Phase 2: Safety gate - need at least one healthy donor
	anyRunning := false
	for _, a := range result.Assessments {
		if a.IsRunning && a.IsReady {
			anyRunning = true
			break
		}
	}
	if !anyRunning {
		return nil, fmt.Errorf("ABORT: No healthy donor nodes found. All nodes are down or unhealthy. " +
			"Cannot heal without a running donor to provide SST")
	}

	// Phase 2.5: Pre-repair escrow backup (always type=backup, follows normal retention)
	if !e.cfg.NoEscrow {
		if e.cfg.BackupsPath == "" || e.cfg.ResticPassword == "" {
			return nil, fmt.Errorf("repair requires --backups-path and RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
		}

		if len(result.DataComparison.PrimaryMembers) > 0 {
			donor := result.DataComparison.PrimaryMembers[0]
			ns := e.cfg.Namespace
			stdinFilename := fmt.Sprintf("%s/%s/%s", ns, e.cfg.ClusterName, DumpFilename)
			escrowResult, err := e.BackupDump(ctx, "backup", donor, stdinFilename, start, nil)
			if err != nil {
				return nil, fmt.Errorf("pre-repair backup failed: %w", err)
			}
			common.InfoLog("Pre-repair backup: %s", escrowResult.SnapshotID)
		} else {
			common.WarnLog("No nodes in Primary component for pre-repair backup. Skipping.")
		}
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
	if e.cfg.InstanceNumber != nil {
		err = e.repairTargeted(ctx, result, repairResult)
	} else {
		err = e.repairUntargeted(ctx, result, repairResult)
	}

	if err != nil {
		return nil, err
	}

	repairResult.Duration = time.Since(start)
	return repairResult, nil
}

func (e *Engine) repairTargeted(ctx context.Context, result *model.TriageResult, repairResult *model.RepairResult) error {
	targetPod := fmt.Sprintf("%s-%d", e.cfg.ClusterName, *e.cfg.InstanceNumber)

	// Find target assessment
	var targetAssessment *model.InstanceAssessment
	for i := range result.Assessments {
		if result.Assessments[i].Pod == targetPod {
			targetAssessment = &result.Assessments[i]
			break
		}
	}
	if targetAssessment == nil {
		return fmt.Errorf("ABORT: %s not found in instance assessments. Check cluster_name and instance_number", targetPod)
	}

	// Safety gate: target is active primary member and healthy
	if targetAssessment.IsInPrimary && !targetAssessment.NeedsHeal && !e.cfg.Force {
		common.WarnLog("%s is an active member of the Primary component. Healing will destroy its data and force SST rejoin.", targetPod)
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
		output.Info("Node %s is healthy and does not need healing. Nothing to do.", targetPod)
		repairResult.SkippedInstances = append(repairResult.SkippedInstances, targetPod)
		return nil
	}
	if !targetAssessment.NeedsHeal && e.cfg.Force {
		common.WarnLog("force=true - healing %s even though it appears healthy", targetPod)
	}

	// Verify storage PVC exists
	c := k8s.GetClients()
	storagePVC := fmt.Sprintf("storage-%s", targetPod)
	_, err := c.Clientset.CoreV1().PersistentVolumeClaims(e.cfg.Namespace).Get(ctx, storagePVC, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("storage PVC %s not found: %w", storagePVC, err)
	}

	// Heal
	if err := e.healNode(ctx, targetPod, *e.cfg.InstanceNumber); err != nil {
		return err
	}
	repairResult.HealedInstances = append(repairResult.HealedInstances, targetPod)

	// Display final status
	e.displayFinalStatus(ctx)
	return nil
}

func (e *Engine) repairUntargeted(ctx context.Context, result *model.TriageResult, repairResult *model.RepairResult) error {
	// Safety gate: split-brain → HARD STOP (no override for untargeted)
	if !result.DataComparison.SafeToHeal {
		return fmt.Errorf("HARD STOP: Split-brain detected. Cannot auto-heal all nodes. " +
			"Admin must review triage output, then use targeted repair: --instance <N>")
	}

	// Compute heal targets
	var targets []model.InstanceAssessment
	for _, a := range result.Assessments {
		if a.NeedsHeal {
			targets = append(targets, a)
		} else {
			repairResult.SkippedInstances = append(repairResult.SkippedInstances, a.Pod)
		}
	}

	if len(targets) == 0 {
		output.Info("All nodes are healthy. Nothing to heal.")
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
		if err := e.healNode(ctx, t.Pod, t.Instance); err != nil {
			return fmt.Errorf("heal failed for %s: %w", t.Pod, err)
		}
		repairResult.HealedInstances = append(repairResult.HealedInstances, t.Pod)
	}

	// Post-repair stabilization
	output.Section("Post-Repair Stabilization")
	common.InfoLog("Waiting 30s for MariaDB operator to reconcile...")
	time.Sleep(30 * time.Second)

	e.waitForAllReady(ctx)

	// Re-triage: re-fetch MariaDB CR state first
	output.Section("Post-Repair Re-Triage")
	obj, err := k8s.GetClients().Dynamic.Resource(k8s.MariaDBGVR).Namespace(e.cfg.Namespace).Get(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err == nil {
		e.mariadb = obj
		e.mariadbSpec = k8s.GetNestedMap(obj, "spec")
		e.mariadbStatus = k8s.GetNestedMap(obj, "status")
		e.readyCondition = findCondition(e.mariadbStatus, "Ready")
		e.galeraCondition = findCondition(e.mariadbStatus, "GaleraReady")
		e.galeraRecovery = k8s.GetNestedMap(obj, "status", "galeraRecovery")
		e.isSuspended = k8s.GetNestedBool(obj, "spec", "suspend")
	}
	postTriage, _ := e.Triage(ctx)
	repairResult.PostTriageResult = postTriage

	return nil
}

func (e *Engine) waitForAllReady(ctx context.Context) {
	c := k8s.GetClients()
	expected := int(e.replicas)

	for i := 0; i < 30; i++ {
		pods, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/instance=" + e.cfg.ClusterName,
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
	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(e.cfg.Namespace).Get(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return
	}

	status := k8s.GetNestedMap(obj, "status")
	readyCond := findCondition(status, "Ready")
	galeraCond := findCondition(status, "GaleraReady")

	output.Section("Final Status")
	if readyCond != nil {
		output.Field("Ready", fmt.Sprintf("%v", readyCond["status"]))
	}
	if galeraCond != nil {
		output.Field("GaleraReady", fmt.Sprintf("%v", galeraCond["status"]))
	}

	pods, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + e.cfg.ClusterName,
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
