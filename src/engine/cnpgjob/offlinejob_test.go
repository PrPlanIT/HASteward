package cnpgjob

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestRun_RestoresReconciliationOnAmbiguousDisableError guards #20: if the
// disable-reconciliation PATCH returns an error (which may still have committed
// server-side), Run must ALWAYS re-enable reconciliation on the way out — never
// leave the whole cluster permanently unreconciled.
func TestRun_RestoresReconciliationOnAmbiguousDisableError(t *testing.T) {
	defer common.DisableSleepForTest()()

	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "pg", "namespace": "ns"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)

	// Fail the "disable" patch (simulating a commit-but-client-errors outcome) and
	// record whether the "re-enable" patch (reconciliationLoop: null) is issued.
	var restoreIssued bool
	dyn.PrependReactor("patch", "clusters", func(a clienttesting.Action) (bool, runtime.Object, error) {
		body := string(a.(clienttesting.PatchAction).GetPatch())
		switch {
		case strings.Contains(body, `"disabled"`):
			return true, nil, fmt.Errorf("injected disable failure (may have committed server-side)")
		case strings.Contains(body, "reconciliationLoop") && strings.Contains(body, "null"):
			restoreIssued = true
		}
		return false, nil, nil
	})

	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: fake.NewSimpleClientset(), Dynamic: dyn})()

	job := OfflinePVCJob{
		Namespace:     "ns",
		ClusterName:   "pg",
		TargetPod:     "pg-2",
		TargetPVC:     "pg-2",
		HelperPodName: "pg-2-clear",
		Label:         "clear",
		HelperPod:     &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-2-clear", Namespace: "ns"}},
	}
	err := Run(context.Background(), job)

	if err == nil {
		t.Fatal("expected Run to fail on the injected disable error")
	}
	if !restoreIssued {
		t.Fatal("reconciliation was NOT re-enabled after an ambiguous disable failure — cluster left unreconciled (#20)")
	}
}

// TestRun_ReturnsErrorWhenUnfenceFails guards #21: if the final unfence fails, the
// instance is STILL FENCED — CNPG will not manage it, so it has NOT rejoined. Run
// must return an error, not swallow it and report success (which would let the
// orchestrator record a dead, unmanaged instance as healed).
func TestRun_ReturnsErrorWhenUnfenceFails(t *testing.T) {
	defer common.DisableSleepForTest()()

	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "pg", "namespace": "ns"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)
	// Let the fence and both reconcile toggles through; fail ONLY the unfence. The
	// unfence patch clears fencedInstances to null — no other patch matches that.
	dyn.PrependReactor("patch", "clusters", func(a clienttesting.Action) (bool, runtime.Object, error) {
		body := string(a.(clienttesting.PatchAction).GetPatch())
		if strings.Contains(body, "fencedInstances") && strings.Contains(body, "null") {
			return true, nil, fmt.Errorf("injected unfence failure")
		}
		return false, nil, nil
	})

	// Auto-succeed the helper pod so Run reaches STEP 6 (the unfence).
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = corev1.PodSucceeded
		}
		return false, nil, nil
	})

	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	job := OfflinePVCJob{
		Namespace:     "ns",
		ClusterName:   "pg",
		TargetPod:     "pg-2",
		TargetPVC:     "pg-2",
		HelperPodName: "pg-2-clear",
		Label:         "clear",
		HelperPod:     &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-2-clear", Namespace: "ns"}},
	}
	err := Run(context.Background(), job)

	if err == nil || !strings.Contains(err.Error(), "still fenced") {
		t.Fatalf("Run must fail when the unfence fails — a still-fenced instance is unmanaged, not healed (#21); got: %v", err)
	}
}

// TestRun_KeepFencedSkipsUnfenceButRestoresReconcile guards the diverged-authority relief:
// with KeepFenced, Run must STILL re-enable reconciliation (the cluster invariant) but must
// NOT unfence the target — leaving the authority frozen so CNPG cannot pg_rewind it onto
// the stale primary's lineage. Success, reconcile re-enabled, no unfence.
func TestRun_KeepFencedSkipsUnfenceButRestoresReconcile(t *testing.T) {
	defer common.DisableSleepForTest()()

	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "pg", "namespace": "ns"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)
	var reconcileReEnabled, unfenceIssued bool
	dyn.PrependReactor("patch", "clusters", func(a clienttesting.Action) (bool, runtime.Object, error) {
		body := string(a.(clienttesting.PatchAction).GetPatch())
		switch {
		case strings.Contains(body, "reconciliationLoop") && strings.Contains(body, "null"):
			reconcileReEnabled = true
		case strings.Contains(body, "fencedInstances") && strings.Contains(body, "null"):
			unfenceIssued = true
		}
		return false, nil, nil
	})

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = corev1.PodSucceeded
		}
		return false, nil, nil
	})

	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	job := OfflinePVCJob{
		Namespace:     "ns",
		ClusterName:   "pg",
		TargetPod:     "pg-2",
		TargetPVC:     "pg-2",
		HelperPodName: "pg-2-wal-prune",
		Label:         "wal-prune",
		KeepFenced:    true,
		HelperPod:     &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-2-wal-prune", Namespace: "ns"}},
	}
	if err := Run(context.Background(), job); err != nil {
		t.Fatalf("KeepFenced success path must not error: %v", err)
	}
	if !reconcileReEnabled {
		t.Fatal("KeepFenced must STILL re-enable reconciliation — leaving the whole cluster unreconciled is the worst outcome")
	}
	if unfenceIssued {
		t.Fatal("KeepFenced must NOT unfence — the diverged authority must be left fenced to protect it from pg_rewind")
	}
}
