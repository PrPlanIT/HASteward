package prunewal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/cnpgjob"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const scratchWALDir = "/scratch/wal"

var (
	volumeSnapshotGVR      = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
	volumeSnapshotClassGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}
)

// deadlockRecover (P3.6) rehabilitates a CNPG instance that is disk-full DEADLOCKED: too
// full to start, so it can never checkpoint to recycle its own WAL, so it stays full —
// a self-sustaining freeze (boundary-postgres-2 sat here for weeks). The fix, proven by
// hand first: escrow the PVC (VolumeSnapshot), fence + isolate the instance, relocate
// pg_wal to ephemeral scratch to free the data disk, run a SINGLE-USER crash-recovery
// REPLAY + CHECKPOINT (which applies all committed WAL — NO data loss — then recycles the
// old segments), pg_archivecleanup the pre-checkpoint deadweight, and move the now-small
// WAL back. The datadir ends clean ("shut down"), de-bloated, on the SAME PV — nothing
// deleted or grown. A trapped non-primary AUTHORITY is KEPT FENCED afterwards (never
// handed back to the operator, which would pg_rewind it onto the stale primary).
func (w *cnpgPruner) deadlockRecover(ctx context.Context, targetPod, targetPVC string, isPrimary bool,
	tr *model.TriageResult, result *model.PruneWALResult) (*model.PruneWALResult, error) {

	cfg := w.p.Config()
	ns := cfg.Namespace
	keepFenced := !isPrimary

	output.Section("Phase 3: Disk-full deadlock recovery (replay + recycle in place)")

	if cfg.DryRun {
		tail := "then unfence and hand it back to the operator"
		if keepFenced {
			tail = "then KEEP IT FENCED (isolated from operator reconcile so it is NOT pg_rewound onto the stale primary)"
		}
		output.Info("DRY RUN: deadlock-recover on %s (PVC %s) — would ESCROW it (VolumeSnapshot), fence + isolate it, "+
			"relocate pg_wal to scratch, run a single-user crash-recovery REPLAY + CHECKPOINT (applies all committed WAL, "+
			"no data loss), pg_archivecleanup the recycled segments, move the small WAL back, %s. No changes made.",
			targetPod, targetPVC, tail)
		return result, nil
	}

	// 1. ESCROW: snapshot the PVC before any mutation. This is the rollback point — the
	//    replay is non-destructive of committed data, but a durable escrow means even a
	//    mid-operation pod death (with WAL on ephemeral scratch) is recoverable. --force
	//    skips it (loudly); refuse rather than proceed blind if no snapshot class is found.
	if cfg.Force {
		common.WarnLog("force=true — SKIPPING the escrow VolumeSnapshot for %s. The replay is non-destructive of committed "+
			"data, but there is NO rollback point if the operation is interrupted.", targetPVC)
	} else {
		snap, err := w.escrowSnapshot(ctx, ns, targetPVC)
		if err != nil {
			return nil, fmt.Errorf("deadlock-recover REFUSED: could not escrow %s before mutating it (fix the "+
				"VolumeSnapshotClass, pass --snapshot-class, or --force to skip escrow): %w", targetPVC, err)
		}
		output.Success("Escrow VolumeSnapshot %s is ready — rollback point captured", snap)
	}

	// 2. Prerequisites. Image comes from the cluster spec (reliable even when no healthy
	//    peer exists); CNPG runs postgres as uid/gid 26. Scratch is sized to the PVC's
	//    capacity (+headroom): the WAL was ON that PVC, so it cannot exceed it.
	imageName := k8s.GetNestedString(w.p.Cluster(), "spec", "imageName")
	if imageName == "" {
		return nil, fmt.Errorf("deadlock-recover: cannot determine cluster imageName for the helper pod")
	}
	scratchSize, err := w.scratchSizeForPVC(ctx, ns, targetPVC)
	if err != nil {
		return nil, fmt.Errorf("deadlock-recover: %w", err)
	}

	uid := int64(26)
	podName := fmt.Sprintf("%s-deadlock-recover-%d", cfg.ClusterName, time.Now().Unix())
	helper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			// A sidecar would strand the helper Running so the PVC-acquire wait never
			// settles (the shared Istio-exemption the helper-pod builder applies).
			Annotations: map[string]string{"sidecar.istio.io/inject": "false"},
			Labels:      map[string]string{"hasteward": "deadlock-recover"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
			},
			Containers: []corev1.Container{{
				Name:    "rehab",
				Image:   imageName,
				Command: []string{"sh", "-c", "sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
					{Name: "scratch", MountPath: "/scratch"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "pgdata", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: targetPVC}}},
				{Name: "scratch", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratchSize}}},
			},
		},
	}

	// 3. Fence → disable reconcile → acquire PVC → (replay+recycle in Go) → re-enable →
	//    keep-fenced-or-unfence, via the shared offline-PVC primitive.
	output.Bullet(0, "Fence, acquire the PVC, replay + recycle WAL, and restore")
	if err := cnpgjob.Run(ctx, cnpgjob.OfflinePVCJob{
		Namespace:        ns,
		ClusterName:      cfg.ClusterName,
		TargetPod:        targetPod,
		TargetPVC:        targetPVC,
		HelperPod:        helper,
		HelperPodName:    podName,
		Label:            "deadlock-recover",
		DeleteTimeoutSec: cfg.DeleteTimeout,
		KeepFenced:       keepFenced,
		OnPVCAcquired: func(ctx context.Context) error {
			return w.deadlockRecoverOnPVC(ctx, podName, ns)
		},
	}); err != nil {
		return nil, err
	}

	if keepFenced {
		output.Success("Deadlock cleared on %s: WAL replayed + recycled, datadir clean and de-bloated on the SAME PV. "+
			"It was KEPT FENCED (isolated) so CNPG cannot start or pg_rewind it onto the stale primary. Promote it "+
			"deliberately; unfence ONLY as part of that promotion (kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-).",
			targetPod, cfg.ClusterName, ns)
		return result, nil
	}
	output.Success("Deadlock cleared on %s: WAL replayed + recycled in place; the operator will restart it on its "+
		"(same, now de-bloated) PVC.", targetPod)
	return result, nil
}

// deadlockRecoverOnPVC runs the replay+recycle sequence via Go-driven exec into the helper
// (which holds the fenced PVC + scratch). Pure over ExecCommand so it is unit-testable with
// the exec hook. Order matters for safety: the WAL is only removed from scratch AFTER the
// replay+checkpoint has applied every committed record to the data files on the PVC.
func (w *cnpgPruner) deadlockRecoverOnPVC(ctx context.Context, helper, ns string) error {
	const c = "rehab"
	// 1. Canonical datadir perms (0700) — an fsGroup mount loosens them and postgres refuses
	//    to start on "invalid permissions". Then relocate pg_wal → scratch (idempotent if a
	//    prior attempt already symlinked it), freeing the data disk for the replay.
	reloc := fmt.Sprintf(`set -e
chmod 700 %[1]s
if [ -L %[1]s/pg_wal ]; then
  echo "pg_wal already relocated"
else
  mkdir -p %[2]s && chmod 700 %[2]s
  mv %[1]s/pg_wal/* %[2]s/ 2>/dev/null || true
  rmdir %[1]s/pg_wal
  ln -s %[2]s %[1]s/pg_wal
fi
df -Pm %[1]s | tail -1`, pgDataDir, scratchWALDir)
	if _, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"sh", "-c", reloc}); err != nil {
		return fmt.Errorf("relocating pg_wal to scratch failed: %w", err)
	}

	// 2. Single-user crash-recovery REPLAY + CHECKPOINT. Bounded WAL so recycling is tight.
	//    Replays every committed record into the data files (no data loss), then a clean
	//    shutdown checkpoint marks the pre-checkpoint WAL recyclable.
	replay := fmt.Sprintf(`echo "CHECKPOINT;" | postgres --single -D %s -c min_wal_size=32MB -c max_wal_size=128MB postgres`, pgDataDir)
	res, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"sh", "-c", replay})
	if err != nil {
		return fmt.Errorf("single-user replay failed (datadir left fenced for inspection): %w", err)
	}
	if strings.Contains(res.Stdout+res.Stderr, "PANIC") || strings.Contains(res.Stdout+res.Stderr, "FATAL") {
		return fmt.Errorf("single-user replay reported PANIC/FATAL — refusing to trim WAL: %s", strings.TrimSpace(res.Stderr))
	}

	// 3. Confirm the instance is now cleanly shut down, and read the new checkpoint REDO WAL
	//    file — the exact cutoff for archivecleanup (everything older is deadweight).
	cd, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"pg_controldata", pgDataDir})
	if err != nil {
		return fmt.Errorf("post-replay pg_controldata failed: %w", err)
	}
	state, redoWAL := parseControlState(cd.Stdout)
	if state != "shut down" {
		return fmt.Errorf("post-replay state is %q, not \"shut down\" — refusing to trim WAL (datadir left fenced)", state)
	}
	if redoWAL == "" {
		return fmt.Errorf("could not read the post-replay checkpoint REDO WAL file — refusing to trim WAL")
	}

	// 4. Remove pre-checkpoint WAL (keeps the REDO segment + newer, preserves .history).
	if _, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"pg_archivecleanup", scratchWALDir, redoWAL}); err != nil {
		return fmt.Errorf("pg_archivecleanup failed: %w", err)
	}

	// 5. Move the now-small WAL back onto the PVC and restore the normal layout.
	moveback := fmt.Sprintf(`set -e
rm %[1]s/pg_wal
mv %[2]s %[1]s/pg_wal
chmod 700 %[1]s/pg_wal
df -Pm %[1]s | tail -1`, pgDataDir, scratchWALDir)
	if _, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"sh", "-c", moveback}); err != nil {
		return fmt.Errorf("moving recycled WAL back onto the PVC failed: %w", err)
	}
	return nil
}

// parseControlState extracts the cluster state and the checkpoint REDO WAL file from
// pg_controldata output.
func parseControlState(out string) (state, redoWAL string) {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Database cluster state:"):
			state = strings.TrimSpace(strings.SplitN(t, ":", 2)[1])
		case strings.Contains(t, "REDO WAL file"):
			redoWAL = lastField(t)
		}
	}
	return state, redoWAL
}

// scratchSizeForPVC sizes the ephemeral scratch volume to the target PVC's capacity plus
// headroom. The relocated WAL lived ON that PVC, so it cannot exceed the PVC's size.
func (w *cnpgPruner) scratchSizeForPVC(ctx context.Context, ns, pvcName string) (resource.Quantity, error) {
	pvc, err := k8s.GetClients().Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("cannot read PVC %s capacity for scratch sizing: %w", pvcName, err)
	}
	cap := pvc.Status.Capacity[corev1.ResourceStorage]
	if cap.IsZero() {
		cap = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	if cap.IsZero() {
		return resource.MustParse("2Gi"), nil // conservative fallback
	}
	out := cap.DeepCopy()
	out.Add(resource.MustParse("512Mi"))
	return out, nil
}

// escrowSnapshot creates a VolumeSnapshot of the PVC and waits until it is readyToUse —
// the rollback point for the in-place replay. The VolumeSnapshotClass is taken from
// --snapshot-class, or discovered by matching the PVC's storage provisioner.
func (w *cnpgPruner) escrowSnapshot(ctx context.Context, ns, pvcName string) (string, error) {
	cfg := w.p.Config()
	c := k8s.GetClients()

	class := cfg.SnapshotClass
	if class == "" {
		var err error
		class, err = w.discoverSnapshotClass(ctx, ns, pvcName)
		if err != nil {
			return "", err
		}
	}

	name := fmt.Sprintf("%s-deadlock-escrow-%d", pvcName, time.Now().Unix())
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name": name, "namespace": ns,
			"labels": map[string]interface{}{"hasteward": "deadlock-escrow", "hasteward.pvc": pvcName},
		},
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": class,
			"source":                  map[string]interface{}{"persistentVolumeClaimName": pvcName},
		},
	}}
	if _, err := c.Dynamic.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("creating VolumeSnapshot (class %s): %w", class, err)
	}

	output.Bullet(0, "Waiting for escrow snapshot %s (class %s) to be ready", name, class)
	for i := 0; i < 60; i++ { // up to 5 min
		common.Sleep(5 * time.Second)
		got, err := c.Dynamic.Resource(volumeSnapshotGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if k8s.GetNestedBool(got, "status", "readyToUse") {
			return name, nil
		}
	}
	return "", fmt.Errorf("VolumeSnapshot %s did not become readyToUse within 5m", name)
}

// discoverSnapshotClass finds a VolumeSnapshotClass whose driver matches the PVC's storage
// provisioner. Errors (rather than guessing) when none is found.
func (w *cnpgPruner) discoverSnapshotClass(ctx context.Context, ns, pvcName string) (string, error) {
	c := k8s.GetClients()
	pvc, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading PVC %s: %w", pvcName, err)
	}
	scName := ""
	if pvc.Spec.StorageClassName != nil {
		scName = *pvc.Spec.StorageClassName
	}
	if scName == "" {
		return "", fmt.Errorf("PVC %s has no StorageClass — pass --snapshot-class explicitly", pvcName)
	}
	sc, err := c.Clientset.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading StorageClass %s: %w", scName, err)
	}
	provisioner := sc.Provisioner

	list, err := c.Dynamic.Resource(volumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing VolumeSnapshotClasses (is the snapshot CRD installed?): %w", err)
	}
	for _, item := range list.Items {
		if driver, ok, _ := unstructured.NestedString(item.Object, "driver"); ok && driver == provisioner {
			return item.GetName(), nil
		}
	}
	return "", fmt.Errorf("no VolumeSnapshotClass matches the PVC provisioner %q — pass --snapshot-class", provisioner)
}
