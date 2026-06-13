package repair

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// offlinePVCJob describes a one-shot helper pod that needs EXCLUSIVE ownership of a
// fenced instance's RWO PVC — to clear it, to pg_basebackup into it, etc. The caller
// supplies the pod spec (what to run) and a completion budget; runOfflinePVCJob owns
// the dangerous part: getting the operator out of the way so the helper can mount the
// volume, and putting it back afterwards.
type offlinePVCJob struct {
	targetPod     string      // the CNPG instance pod whose PVC we need
	targetPVC     string      // its PVC (already mounted by job.helperPod's spec)
	helperPod     *corev1.Pod // caller-built; mounts targetPVC, runs the work, then exits
	helperPodName string
	label         string // "heal" | "clear" — used only in log lines
	completeSec   int    // budget for the helper to reach Succeeded (default 600)
}

// runOfflinePVCJob runs a helper pod that must take over a fenced instance's RWO PVC.
//
// The historical implementation "aggressively deleted" the instance pod in a 300s loop
// hoping to win a race for the PVC. That race is UNWINNABLE on a healthy cluster: CNPG
// fencing stops PostgreSQL but does not stop the operator from recreating the instance
// pod, which re-grabs the RWO volume before the helper can. (Validated on the live
// fleet 2026-06-13: a fenced, deleted pod was recreated within ~1s, every time.)
//
// The fix is to stop the operator from reconciling for the duration of the handoff:
// with cnpg.io/reconciliationLoop=disabled the deleted instance pod STAYS gone (proven:
// 100s, zero recreation), so the helper acquires the PVC with a single delete. This is
// the existing PVC-preserving primitive — no PVC/PV deletion, no storage churn — minus
// the race.
//
// SAFETY: the reconciliation loop is re-enabled on EVERY exit path via defer. Leaving a
// cluster unreconciled is far more dangerous than a left-in-place fence, so re-enable is
// unconditional; the fence, by contrast, is intentionally left on failure so the
// operator does not start a half-written datadir.
func (r *cnpgRepair) runOfflinePVCJob(ctx context.Context, job offlinePVCJob) error {
	cfg := r.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	helperCreated := false
	fenceApplied := false

	cleanup := func() {
		if helperCreated {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, job.helperPodName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			common.InfoLog("%s pod %s deleted", job.label, job.helperPodName)
		}
		if fenceApplied {
			common.WarnLog("%s FAILED - fence left in place for safety. Instance %s is still fenced.",
				strings.ToUpper(job.label), job.targetPod)
			common.WarnLog("To remove fence: kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-", cfg.ClusterName, ns)
		}
	}

	// STEP 1: Fence the instance. Fencing is operator-driven, so it must be applied
	// while reconciliation is still ENABLED (below) for the operator to act on it.
	common.InfoLog("STEP 1: Fencing %s", job.targetPod)
	if err := r.fenceInstance(ctx, job.targetPod); err != nil {
		return fmt.Errorf("failed to fence %s: %w", job.targetPod, err)
	}
	fenceApplied = true
	time.Sleep(3 * time.Second)

	// STEP 2: Disable the reconciliation loop so the operator stops recreating the
	// instance pod (the unwinnable PVC race). ALWAYS re-enabled on return.
	if err := r.setReconciliationLoop(ctx, true /* disabled */); err != nil {
		cleanup()
		return fmt.Errorf("failed to disable reconciliation loop: %w", err)
	}
	defer func() {
		if err := r.setReconciliationLoop(ctx, false /* re-enabled */); err != nil {
			common.WarnLog("CRITICAL: failed to re-enable reconciliation loop on cluster %s: %v", cfg.ClusterName, err)
			common.WarnLog("Re-enable manually: kubectl annotate cluster %s -n %s cnpg.io/reconciliationLoop-", cfg.ClusterName, ns)
		}
	}()

	// STEP 3: Create the helper pod. It stays Pending until the PVC is free.
	common.InfoLog("STEP 2: Creating %s pod %s", job.label, job.helperPodName)
	if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, job.helperPod, metav1.CreateOptions{}); err != nil {
		cleanup()
		return fmt.Errorf("failed to create %s pod: %w", job.label, err)
	}
	helperCreated = true
	time.Sleep(2 * time.Second)

	// STEP 4: Free the PVC. With reconciliation disabled, one delete is enough — the
	// operator will not recreate the instance pod — but the RWO volume detach/attach
	// (Ceph RBD unmap→map) can lag, so we poll for acquisition and re-issue the delete
	// only if the pod is still present (e.g. mid-termination).
	common.InfoLog("STEP 3: Deleting %s (reconcile disabled — it stays down) so the %s pod can acquire the PVC",
		job.targetPod, job.label)
	deleteTimeout := cfg.DeleteTimeout
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}
	acquired := false
	for elapsed := 0; elapsed < deleteTimeout; elapsed++ {
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, job.helperPodName, metav1.GetOptions{})
		phase := "Pending"
		if hpErr == nil {
			phase = string(hp.Status.Phase)
		}
		if phase == "Running" || phase == "Succeeded" {
			common.InfoLog("%s pod acquired the PVC", job.label)
			acquired = true
			break
		}
		if phase == "Failed" {
			r.logHealPodOutput(ctx, job.helperPodName)
			cleanup()
			return fmt.Errorf("%s pod failed before acquiring PVC", job.label)
		}
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, job.targetPod, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
		time.Sleep(1 * time.Second)
	}
	if !acquired {
		r.logHealPodOutput(ctx, job.helperPodName)
		cleanup()
		return fmt.Errorf("timeout: %s pod never acquired PVC after %ds", job.label, deleteTimeout)
	}

	// STEP 5: Wait for the helper to finish its work.
	common.InfoLog("STEP 4: Waiting for the %s pod to complete", job.label)
	completeTimeout := job.completeSec
	if completeTimeout <= 0 {
		completeTimeout = 600
	}
	succeeded := false
	for i := 0; i < completeTimeout/5; i++ {
		time.Sleep(5 * time.Second)
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, job.helperPodName, metav1.GetOptions{})
		if hpErr != nil {
			continue
		}
		switch hp.Status.Phase {
		case corev1.PodSucceeded:
			succeeded = true
		case corev1.PodFailed:
			r.logHealPodOutput(ctx, job.helperPodName)
			cleanup()
			return fmt.Errorf("%s pod FAILED for %s", job.label, job.targetPod)
		}
		if succeeded {
			break
		}
	}
	if !succeeded {
		r.logHealPodOutput(ctx, job.helperPodName)
		cleanup()
		return fmt.Errorf("%s pod timed out after %ds for %s", job.label, completeTimeout, job.targetPod)
	}

	r.logHealPodOutput(ctx, job.helperPodName)

	// Drop the helper pod, releasing the PVC.
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, job.helperPodName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	helperCreated = false
	time.Sleep(3 * time.Second)

	// STEP 6: Unfence. On return the deferred reconcile re-enable lets the operator
	// recreate and start the instance on its preserved (now-prepared) PVC. There is no
	// stale instance pod to clean up — reconciliation was off, so none was ever made.
	common.InfoLog("STEP 5: Removing fence for %s", job.targetPod)
	if err := r.unfenceInstance(ctx, job.targetPod); err != nil {
		common.WarnLog("Failed to unfence %s: %v", job.targetPod, err)
	}
	fenceApplied = false

	return nil
}

// setReconciliationLoop toggles the CNPG cluster-wide reconciliation loop via the
// cnpg.io/reconciliationLoop annotation (operator >= v1.11.0). Disabling it stops the
// operator from recreating deleted instance pods — the only reliable way to hand an
// instance's RWO PVC to an offline helper pod without racing the operator. It is
// CLUSTER-scoped, so callers must keep the disabled window as tight as possible and
// re-enable on every exit path.
func (r *cnpgRepair) setReconciliationLoop(ctx context.Context, disabled bool) error {
	cfg := r.p.Config()
	c := k8s.GetClients()

	var patch string
	if disabled {
		patch = `{"metadata":{"annotations":{"cnpg.io/reconciliationLoop":"disabled"}}}`
		common.InfoLog("Disabling reconciliation loop on %s (operator will not recreate the instance pod)", cfg.ClusterName)
	} else {
		patch = `{"metadata":{"annotations":{"cnpg.io/reconciliationLoop":null}}}`
		common.InfoLog("Re-enabling reconciliation loop on %s", cfg.ClusterName)
	}

	_, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}
