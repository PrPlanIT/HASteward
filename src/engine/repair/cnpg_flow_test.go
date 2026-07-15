package repair

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestCNPGHealInstance_NeverReadyReturnsError guards #19 (the CNPG mirror of #7):
// the offline-PVC job completes, but the operator never brings the replica back
// Ready, so healInstance must return an ERROR — not nil — so the orchestrator
// never records an un-rejoined replica as healed.
func TestCNPGHealInstance_NeverReadyReturnsError(t *testing.T) {
	ctx := context.Background()
	defer common.DisableSleepForTest()()

	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "pg", "namespace": "ns"},
		"spec":       map[string]interface{}{"instances": int64(3)},
		"status":     map[string]interface{}{"currentPrimary": "pg-1"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)
	// Auto-succeed the offline-job helper pod so cnpgjob.Run completes and we reach
	// the post-heal readiness wait. The target replica pg-2 is never created Ready.
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = corev1.PodSucceeded
		}
		return false, nil, nil
	})

	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	p := provider.NewCNPGProviderForTest(
		&common.Config{ClusterName: "pg", Namespace: "ns", HealTimeout: 10, DeleteTimeout: 5}, 3, cluster)
	r := &cnpgRepair{p: p}
	hcfg := &healConfig{primaryIP: "10.0.0.1", postgresUID: "26", postgresGID: "26", imageName: "pg:16", serviceAccount: "default"}

	err := r.healInstance(ctx, "pg-2", "pg-2", hcfg)
	if err == nil {
		t.Fatal("healInstance returned nil for a replica that never became Ready — must return an error (#19)")
	}
	if !strings.Contains(err.Error(), "did not become Ready") {
		t.Fatalf("unexpected error (want a not-Ready failure): %v", err)
	}
}

// TestCNPGHealInstance_RefusesToHealPrimary guards #22: an untargeted repair must
// never fence + rm -rf the datadir of the CURRENT PRIMARY. healInstance must refuse
// BEFORE any destructive step (no helper pod, no fence) when the target is primary.
func TestCNPGHealInstance_RefusesToHealPrimary(t *testing.T) {
	ctx := context.Background()
	defer common.DisableSleepForTest()()

	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "pg", "namespace": "ns"},
		"spec":       map[string]interface{}{"instances": int64(3)},
		"status":     map[string]interface{}{"currentPrimary": "pg-1"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)
	// If the guard fails to fire, the very first destructive step creates a helper pod.
	var podCreated bool
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		podCreated = true
		return false, nil, nil
	})

	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	p := provider.NewCNPGProviderForTest(
		&common.Config{ClusterName: "pg", Namespace: "ns", HealTimeout: 10, DeleteTimeout: 5}, 3, cluster)
	r := &cnpgRepair{p: p}
	hcfg := &healConfig{primaryIP: "10.0.0.1", postgresUID: "26", postgresGID: "26", imageName: "pg:16", serviceAccount: "default"}

	// pg-1 IS the current primary — healing it would destroy the primary's datadir.
	err := r.healInstance(ctx, "pg-1", "pg-1", hcfg)
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("healInstance must refuse to heal the current primary (#22), got: %v", err)
	}
	if podCreated {
		t.Fatal("healInstance created a helper pod for the primary — the guard must abort BEFORE any destructive step (#22)")
	}
}
