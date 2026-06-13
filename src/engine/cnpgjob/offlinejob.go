// Package cnpgjob provides the shared "offline PVC job" primitive used by every
// HASteward operation that needs EXCLUSIVE ownership of a CNPG instance's RWO PVC
// while the instance is down — repair (clear + pg_basebackup), the deadlock breaker
// (clear-only), and WAL pruning. It owns the dangerous part once: getting the operator
// out of the way so a helper pod can mount the volume, and putting it back afterwards.
package cnpgjob

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// OfflinePVCJob describes a one-shot helper pod that needs EXCLUSIVE ownership of a
// fenced instance's RWO PVC — to clear it, to pg_basebackup into it, to prune its WAL,
// etc. The caller supplies the pod spec (what to run) and timeouts; Run owns the
// fence → reconcile-disable → acquire → reconcile-re-enable → unfence dance.
type OfflinePVCJob struct {
	Namespace     string
	ClusterName   string
	TargetPod     string      // the CNPG instance pod whose PVC we need
	TargetPVC     string      // its PVC (already referenced by HelperPod's spec)
	HelperPod     *corev1.Pod // caller-built; mounts TargetPVC, runs the work, then exits
	HelperPodName string
	Label         string // "heal" | "clear" | "wal-prune" — used only in log lines
	// DeleteTimeoutSec bounds how long to wait for the helper to acquire the PVC
	// (default 300). CompleteTimeoutSec bounds the helper's own work (default 600).
	DeleteTimeoutSec   int
	CompleteTimeoutSec int
}

// Run runs a helper pod that must take over a fenced instance's RWO PVC.
//
// The historical implementation "aggressively deleted" the instance pod in a 300s loop
// hoping to win a race for the PVC. That race is UNWINNABLE on a healthy cluster: CNPG
// fencing stops PostgreSQL but does not stop the operator from recreating the instance
// pod, which re-grabs the RWO volume before the helper can. (Validated on the live fleet
// 2026-06-13: a fenced, deleted pod was recreated within ~1s, every time.)
//
// The fix is to stop the operator from reconciling for the duration of the handoff:
// with cnpg.io/reconciliationLoop=disabled the deleted instance pod STAYS gone (proven:
// 100s, zero recreation), so the helper acquires the PVC with a single delete. No
// PVC/PV deletion, no storage churn — the existing PVC-preserving primitive, minus the
// race.
//
// SAFETY: the reconciliation loop is re-enabled on EVERY exit path via defer. Leaving a
// cluster unreconciled is far more dangerous than a left-in-place fence, so re-enable is
// unconditional; the fence, by contrast, is intentionally left on failure so the
// operator does not start a half-written datadir.
func Run(ctx context.Context, job OfflinePVCJob) error {
	c := k8s.GetClients()
	ns := job.Namespace

	helperCreated := false
	fenceApplied := false
	reconcileDisabled := false

	cleanup := func() {
		if helperCreated {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, job.HelperPodName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			common.InfoLog("%s pod %s deleted", job.Label, job.HelperPodName)
		}
		if fenceApplied {
			common.WarnLog("%s FAILED - fence left in place for safety. Instance %s is still fenced.",
				strings.ToUpper(job.Label), job.TargetPod)
			common.WarnLog("To remove fence: kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-", job.ClusterName, ns)
		}
	}

	// STEP 1: Fence the instance. Fencing is operator-driven, so it must be applied
	// while reconciliation is still ENABLED (below) for the operator to act on it.
	common.InfoLog("STEP 1: Fencing %s", job.TargetPod)
	if err := Fence(ctx, ns, job.ClusterName, job.TargetPod); err != nil {
		return fmt.Errorf("failed to fence %s: %w", job.TargetPod, err)
	}
	fenceApplied = true
	time.Sleep(3 * time.Second)

	// STEP 2: Disable the reconciliation loop so the operator stops recreating the
	// instance pod (the unwinnable PVC race). The deferred restore is the GUARANTEED
	// safety net: it runs on every exit path — including panic and, crucially, context
	// cancellation (via a DETACHED context) — and retries, because a cluster left with
	// reconciliation disabled is the worst outcome this primitive can produce.
	if err := SetReconciliationLoop(ctx, ns, job.ClusterName, true /* disabled */); err != nil {
		cleanup()
		return fmt.Errorf("failed to disable reconciliation loop: %w", err)
	}
	reconcileDisabled = true
	defer restoreReconciliation(job.ClusterName, ns, &reconcileDisabled)

	// STEP 3: Create the helper pod. It stays Pending until the PVC is free.
	common.InfoLog("STEP 2: Creating %s pod %s", job.Label, job.HelperPodName)
	if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, job.HelperPod, metav1.CreateOptions{}); err != nil {
		cleanup()
		return fmt.Errorf("failed to create %s pod: %w", job.Label, err)
	}
	helperCreated = true
	time.Sleep(2 * time.Second)

	// STEP 4: Free the PVC. With reconciliation disabled, one delete is enough — the
	// operator will not recreate the instance pod — but the RWO volume detach/attach
	// (Ceph RBD unmap→map) can lag, so we poll for acquisition and re-issue the delete
	// only if the pod is still present (e.g. mid-termination).
	common.InfoLog("STEP 3: Deleting %s (reconcile disabled — it stays down) so the %s pod can acquire the PVC",
		job.TargetPod, job.Label)
	deleteTimeout := job.DeleteTimeoutSec
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}
	acquired := false
	for elapsed := 0; elapsed < deleteTimeout; elapsed++ {
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, job.HelperPodName, metav1.GetOptions{})
		phase := "Pending"
		if hpErr == nil {
			phase = string(hp.Status.Phase)
		}
		if phase == "Running" || phase == "Succeeded" {
			common.InfoLog("%s pod acquired the PVC", job.Label)
			acquired = true
			break
		}
		if phase == "Failed" {
			logHelperOutput(ctx, ns, job.HelperPodName)
			cleanup()
			return fmt.Errorf("%s pod failed before acquiring PVC", job.Label)
		}
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, job.TargetPod, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
		time.Sleep(1 * time.Second)
	}
	if !acquired {
		logHelperOutput(ctx, ns, job.HelperPodName)
		cleanup()
		return fmt.Errorf("timeout: %s pod never acquired PVC after %ds", job.Label, deleteTimeout)
	}

	// STEP 5: Wait for the helper to finish its work.
	common.InfoLog("STEP 4: Waiting for the %s pod to complete", job.Label)
	completeTimeout := job.CompleteTimeoutSec
	if completeTimeout <= 0 {
		completeTimeout = 600
	}
	succeeded := false
	for i := 0; i < completeTimeout/5; i++ {
		time.Sleep(5 * time.Second)
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, job.HelperPodName, metav1.GetOptions{})
		if hpErr != nil {
			continue
		}
		switch hp.Status.Phase {
		case corev1.PodSucceeded:
			succeeded = true
		case corev1.PodFailed:
			logHelperOutput(ctx, ns, job.HelperPodName)
			cleanup()
			return fmt.Errorf("%s pod FAILED for %s", job.Label, job.TargetPod)
		}
		if succeeded {
			break
		}
	}
	if !succeeded {
		logHelperOutput(ctx, ns, job.HelperPodName)
		cleanup()
		return fmt.Errorf("%s pod timed out after %ds for %s", job.Label, completeTimeout, job.TargetPod)
	}

	logHelperOutput(ctx, ns, job.HelperPodName)

	// Drop the helper pod, releasing the PVC.
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, job.HelperPodName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	helperCreated = false
	time.Sleep(3 * time.Second)

	// STEP 6: Restore the operator BEFORE handing the instance back. Re-enabling
	// reconciliation first exits the hazardous "reconcile disabled" state as early as
	// possible: if anything dies between here and the unfence, the cluster is left in the
	// SAFE state (reconcile on, instance still fenced) rather than the dangerous one
	// (reconcile off, instance live). On success this makes the deferred restore a no-op.
	common.InfoLog("STEP 5: Re-enabling reconciliation on %s", job.ClusterName)
	if err := SetReconciliationLoop(ctx, ns, job.ClusterName, false /* re-enabled */); err != nil {
		// Leave reconcileDisabled=true so the deferred safety net retries on a detached ctx.
		common.WarnLog("re-enable on the success path failed; deferred safety net will retry: %v", err)
	} else {
		reconcileDisabled = false
	}

	// STEP 7: Unfence — hand the instance to the now-live operator, which recreates and
	// starts it on its preserved (now-prepared) PVC. No stale instance pod to clean up —
	// reconciliation was off during the handoff, so none was ever made.
	common.InfoLog("STEP 6: Removing fence for %s", job.TargetPod)
	if err := Unfence(ctx, ns, job.ClusterName, job.TargetPod); err != nil {
		common.WarnLog("Failed to unfence %s: %v", job.TargetPod, err)
	}
	fenceApplied = false

	return nil
}

// restoreReconciliation re-enables the reconciliation loop as a GUARANTEED safety net,
// using a DETACHED context so it runs even when the caller's ctx was cancelled — the very
// situation in which an inline re-enable would also fail and silently leave the cluster
// unreconciled. It retries, and no-ops if the success path already restored it.
func restoreReconciliation(cluster, ns string, stillDisabled *bool) {
	if !*stillDisabled {
		return
	}
	for attempt := 1; attempt <= 5; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := SetReconciliationLoop(ctx, ns, cluster, false /* re-enabled */)
		cancel()
		if err == nil {
			*stillDisabled = false
			return
		}
		common.WarnLog("attempt %d/5: failed to re-enable reconciliation on %s: %v", attempt, cluster, err)
		time.Sleep(3 * time.Second)
	}
	common.WarnLog("CRITICAL: could not re-enable reconciliation on cluster %s after 5 attempts.", cluster)
	common.WarnLog("Re-enable manually: kubectl annotate cluster %s -n %s cnpg.io/reconciliationLoop-", cluster, ns)
}

// Fence appends a pod to the cluster's cnpg.io/fencedInstances annotation.
func Fence(ctx context.Context, ns, cluster, pod string) error {
	c := k8s.GetClients()
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		return err
	}
	current := provider.ParseFencedInstances(k8s.GetNestedMap(obj, "metadata", "annotations"))
	for _, f := range current {
		if f == pod {
			common.InfoLog("Instance %s already fenced", pod)
			return nil
		}
	}
	fencedJSON, _ := json.Marshal(append(current, pod))
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"cnpg.io/fencedInstances":%q}}}`, string(fencedJSON))
	_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Patch(
		ctx, cluster, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// Unfence removes a pod from the cluster's cnpg.io/fencedInstances annotation,
// dropping the annotation entirely when no instances remain fenced.
func Unfence(ctx context.Context, ns, cluster, pod string) error {
	c := k8s.GetClients()
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		return err
	}
	current := provider.ParseFencedInstances(k8s.GetNestedMap(obj, "metadata", "annotations"))
	var remaining []string
	for _, f := range current {
		if f != pod {
			remaining = append(remaining, f)
		}
	}
	var patch string
	if len(remaining) == 0 {
		patch = `{"metadata":{"annotations":{"cnpg.io/fencedInstances":null}}}`
	} else {
		fencedJSON, _ := json.Marshal(remaining)
		patch = fmt.Sprintf(`{"metadata":{"annotations":{"cnpg.io/fencedInstances":%q}}}`, string(fencedJSON))
	}
	_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Patch(
		ctx, cluster, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// SetReconciliationLoop toggles the CNPG cluster-wide reconciliation loop via the
// cnpg.io/reconciliationLoop annotation (operator >= v1.11.0). Disabling it stops the
// operator from recreating deleted instance pods — the only reliable way to hand an
// instance's RWO PVC to an offline helper pod without racing the operator. It is
// CLUSTER-scoped, so callers must keep the disabled window as tight as possible and
// re-enable on every exit path.
func SetReconciliationLoop(ctx context.Context, ns, cluster string, disabled bool) error {
	c := k8s.GetClients()
	var patch string
	if disabled {
		patch = `{"metadata":{"annotations":{"cnpg.io/reconciliationLoop":"disabled"}}}`
		common.InfoLog("Disabling reconciliation loop on %s (operator will not recreate the instance pod)", cluster)
	} else {
		patch = `{"metadata":{"annotations":{"cnpg.io/reconciliationLoop":null}}}`
		common.InfoLog("Re-enabling reconciliation loop on %s", cluster)
	}
	_, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Patch(
		ctx, cluster, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// logHelperOutput fetches and displays logs from a helper pod (best-effort).
func logHelperOutput(ctx context.Context, ns, podName string) {
	c := k8s.GetClients()
	stream, err := c.Clientset.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		common.DebugLog("Failed to get %s logs: %v", podName, err)
		return
	}
	defer stream.Close()
	if data, _ := io.ReadAll(stream); len(data) > 0 {
		common.InfoLog("Helper pod output:\n%s", string(data))
	}
}

func ptr[T any](v T) *T { return &v }
