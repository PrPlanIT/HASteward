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
// Preference order:
//  1. restic PVC-files backup — if a restic repo is configured. It DEDUPLICATES, and
//     the repository is wherever the operator pointed --backups-path, which is how an
//     escrow stays off the primary storage tier it is protecting. A CoW snapshot
//     instead lives in the same pool as the volume it protects and grows as that
//     volume diverges from it, so making it the default put escrows on the fast tier
//     and left nothing bounding them.
//  2. CSI VolumeSnapshot — when no restic repository is configured, or the recovery
//     set cannot be reached by a helper pod. The CSI layer asserts restorability
//     itself and needs no mount, so it remains the fallback that always works.
//  3. refuse.
func Select(ctx context.Context, cfg *common.Config, kind string, recoverySet []string) (EscrowProvider, error) {
	if len(recoverySet) == 0 {
		return nil, fmt.Errorf("escrow: empty recovery set — nothing to make reversible")
	}

	runID := NewRunID()

	if cfg.BackupsPath != "" && cfg.ResticPassword != "" {
		return newResticPVCEscrow(cfg, kind, runID), nil
	}

	// --snapshot-class is an explicit operator choice, so it skips discovery entirely.
	if cfg.SnapshotClass != "" {
		return newVolumeSnapshotEscrow(cfg, cfg.SnapshotClass, kind, runID), nil
	}
	if class, err := matchSnapshotClass(ctx, cfg.Namespace, recoverySet[0]); err == nil && class != "" {
		return newVolumeSnapshotEscrow(cfg, class, kind, runID), nil
	}

	return nil, fmt.Errorf(
		"escrow unavailable: no restic repo is configured (--backups-path + restic password) and no VolumeSnapshotClass matches the recovery set's storage provisioner — reversibility cannot be proven, so the operation is refused")
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
