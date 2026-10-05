package escrow

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Select chooses the escrow provider FAIL-CLOSED: it returns a provider only when
// that provider can prove reversibility, and refuses otherwise. The safety
// invariant is verified reversibility, so "no provable provider" must mean "the
// breaker is unavailable", never "escrow silently skipped".
//
// The choice turns on whether the data is QUIESCENT, because the two providers differ in
// what they can promise:
//
//   - A CSI VolumeSnapshot is atomic at the block layer, so it is the only safe capture
//     of a volume something is still writing to. Its cost is placement: a CoW snapshot
//     lives in the same pool as the volume it protects and grows as that volume diverges,
//     so it consumes the very tier the database runs on.
//   - A restic PVC backup tars the datadir file by file with no quiesce, so against a
//     live writer it can capture a torn copy — and Verify proves only that the archive
//     retrieves and parses, not that PostgreSQL could recover it. Against a stopped
//     instance there is nothing to tear, and it deduplicates into whatever store
//     --backups-path names, which keeps the escrow off the database's own tier.
//
// So: a recovery set with any live writer takes the snapshot, and a fully quiescent one
// takes restic. That is not a compromise between the two — it is each where it is sound.
// Preferring restic unconditionally would trade escrow integrity for tier placement.
func Select(ctx context.Context, cfg *common.Config, kind string, recoverySet []string) (EscrowProvider, error) {
	if len(recoverySet) == 0 {
		return nil, fmt.Errorf("escrow: empty recovery set — nothing to make reversible")
	}

	runID := NewRunID()
	resticOK := cfg.BackupsPath != "" && cfg.ResticPassword != ""

	if resticOK && !anyLiveWriter(ctx, cfg.Namespace, recoverySet) {
		return newResticPVCEscrow(cfg, kind, runID), nil
	}

	// --snapshot-class is an explicit operator choice, so it skips discovery entirely.
	if cfg.SnapshotClass != "" {
		return newVolumeSnapshotEscrow(cfg, cfg.SnapshotClass, kind, runID), nil
	}
	if class, err := matchSnapshotClass(ctx, cfg.Namespace, recoverySet[0]); err == nil && class != "" {
		return newVolumeSnapshotEscrow(cfg, class, kind, runID), nil
	}

	// No atomic capture available. Restic is still better than no escrow at all, but the
	// caller must know the copy may be torn rather than discover it at restore time.
	if resticOK {
		common.WarnLog("escrow: no VolumeSnapshotClass matches %v and something is still writing to it — "+
			"falling back to a restic file copy, which is NOT atomic and may be torn", recoverySet)
		return newResticPVCEscrow(cfg, kind, runID), nil
	}

	return nil, fmt.Errorf(
		"escrow unavailable: no restic repo is configured (--backups-path + restic password) and no VolumeSnapshotClass matches the recovery set's storage provisioner — reversibility cannot be proven, so the operation is refused")
}

// anyLiveWriter reports whether any PVC in the set is mounted by a pod with a READY
// container — the test for "something may be writing to this right now".
//
// Readiness, not pod phase: a crash-looping instance sits in phase Running while its
// postgres never finishes recovery, and that datadir is quiescent precisely because
// nothing is serving from it. Judging by phase would send the commonest escrow case —
// the instance that will not start — to the snapshot provider and back onto the
// database's own storage tier.
//
// Unknown is treated as live. A listing we could not read is not evidence of quiet.
func anyLiveWriter(ctx context.Context, ns string, pvcs []string) bool {
	want := make(map[string]bool, len(pvcs))
	for _, p := range pvcs {
		want[p] = true
	}
	pods, err := k8s.GetClients().Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return true
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		mounts := false
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && want[v.PersistentVolumeClaim.ClaimName] {
				mounts = true
				break
			}
		}
		if !mounts {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				return true
			}
		}
	}
	return false
}

// matchSnapshotClass returns the name of a VolumeSnapshotClass whose driver
// matches the PVC's StorageClass provisioner, or "" if none. The driver↔provisioner
// match is what makes a CSI snapshot of this PVC actually possible.
func matchSnapshotClass(ctx context.Context, ns, pvcName string) (string, error) {
	c := k8s.GetClients()

	pvc, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pvc %s: %w", pvcName, err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", fmt.Errorf("pvc %s has no explicit storageClassName", pvcName)
	}

	sc, err := c.Clientset.StorageV1().StorageClasses().Get(ctx, *pvc.Spec.StorageClassName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get storageclass %s: %w", *pvc.Spec.StorageClassName, err)
	}
	provisioner := sc.Provisioner

	list, err := c.Dynamic.Resource(k8s.VolumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list volumesnapshotclasses: %w", err)
	}
	for i := range list.Items {
		item := list.Items[i]
		if driverOf(&item) == provisioner {
			return item.GetName(), nil
		}
	}
	return "", fmt.Errorf("no VolumeSnapshotClass with driver %q", provisioner)
}

// driverOf reads the top-level .driver of a VolumeSnapshotClass.
func driverOf(obj *unstructured.Unstructured) string {
	return k8s.GetNestedString(obj, "driver")
}

// shortID is the human-discoverable prefix of a run id, used in object names.
func shortID(runID string) string {
	if len(runID) > 8 {
		return runID[:8]
	}
	return runID
}

// escrowName is the discoverable VolumeSnapshot name for one escrowed PVC.
func escrowName(pvc, runID string) string {
	return fmt.Sprintf("hasteward-escrow-%s-%s", pvc, shortID(runID))
}
