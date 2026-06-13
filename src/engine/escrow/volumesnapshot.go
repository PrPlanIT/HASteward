package escrow

import (
	"context"
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// escrowLabelPrefix namespaces the discovery labels stamped on every escrow.
const escrowLabelPrefix = "hasteward.prplanit.com/"

// volumeSnapshotEscrow escrows PVCs as CSI VolumeSnapshots. Verify gates on the
// snapshot's own readyToUse signal — the CSI driver's assertion that the snapshot
// is restorable — which is the storage layer's authoritative restorability proof.
type volumeSnapshotEscrow struct {
	cfg   *common.Config
	class string // selected VolumeSnapshotClass whose driver matches the PVCs' provisioner
	runID string
}

func newVolumeSnapshotEscrow(cfg *common.Config, class, runID string) *volumeSnapshotEscrow {
	return &volumeSnapshotEscrow{cfg: cfg, class: class, runID: runID}
}

func (e *volumeSnapshotEscrow) Name() string { return "volumesnapshot" }

func (e *volumeSnapshotEscrow) Capture(ctx context.Context, recoverySet []string) ([]EscrowRef, error) {
	c := k8s.GetClients()
	ns := e.cfg.Namespace
	now := time.Now()

	output.Section("Escrow Capture (volumesnapshot)")
	output.Field("Class", e.class)

	refs := make([]EscrowRef, 0, len(recoverySet))
	for _, pvc := range recoverySet {
		name := escrowName(pvc, e.runID)
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "hasteward",
					escrowLabelPrefix + "escrow":   "true",
					escrowLabelPrefix + "cluster":  e.cfg.ClusterName,
					escrowLabelPrefix + "instance": pvc,
					escrowLabelPrefix + "run-id":   e.runID,
				},
				// Timestamp lives in an annotation: RFC3339 contains ':' which is
				// not a legal label value.
				"annotations": map[string]interface{}{
					escrowLabelPrefix + "captured-at": now.UTC().Format(time.RFC3339),
				},
			},
			"spec": map[string]interface{}{
				"volumeSnapshotClassName": e.class,
				"source": map[string]interface{}{
					"persistentVolumeClaimName": pvc,
				},
			},
		}}

		if _, err := c.Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return refs, fmt.Errorf("failed to create VolumeSnapshot %s for pvc %s: %w", name, pvc, err)
		}
		common.InfoLog("Captured VolumeSnapshot %s for pvc %s", name, pvc)

		refs = append(refs, EscrowRef{
			Provider:  e.Name(),
			ID:        name,
			PVC:       pvc,
			Cluster:   e.cfg.ClusterName,
			Instance:  pvc, // CNPG pgdata PVC is named after its instance
			RunID:     e.runID,
			CreatedAt: now,
		})
	}
	return refs, nil
}

func (e *volumeSnapshotEscrow) Verify(ctx context.Context, refs []EscrowRef) error {
	c := k8s.GetClients()
	ns := e.cfg.Namespace

	// Bounded poll: readyToUse is the CSI driver's restorability assertion.
	const (
		attempts = 60
		interval = 5 * time.Second
	)
	for _, ref := range refs {
		ready := false
		for i := 0; i < attempts; i++ {
			obj, err := c.Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(ns).Get(ctx, ref.ID, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("escrow %s: cannot read VolumeSnapshot: %w", ref.ID, err)
			}
			// A reported snapshot error is terminal — never wait it out.
			if msg := k8s.GetNestedString(obj, "status", "error", "message"); msg != "" {
				return fmt.Errorf("escrow %s (pvc %s) failed: VolumeSnapshot error: %s", ref.ID, ref.PVC, msg)
			}
			if k8s.GetNestedBool(obj, "status", "readyToUse") {
				ready = true
				break
			}
			time.Sleep(interval)
		}
		if !ready {
			return fmt.Errorf("escrow %s (pvc %s) never became readyToUse within %s — restorability unproven, refuse to proceed",
				ref.ID, ref.PVC, time.Duration(attempts)*interval)
		}
		common.InfoLog("Verified VolumeSnapshot %s is readyToUse (pvc %s)", ref.ID, ref.PVC)
	}
	return nil
}

func (e *volumeSnapshotEscrow) Cleanup(ctx context.Context, refs []EscrowRef) error {
	c := k8s.GetClients()
	ns := e.cfg.Namespace
	for _, ref := range refs {
		err := c.Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(ns).Delete(ctx, ref.ID, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			common.WarnLog("Failed to delete escrow VolumeSnapshot %s: %v", ref.ID, err)
		}
	}
	return nil
}
