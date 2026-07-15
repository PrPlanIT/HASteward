package provider

import (
	"context"
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
)

func testProvider() *GaleraProvider {
	return &GaleraProvider{cfg: &common.Config{ClusterName: "c", Namespace: "ns"}}
}

func mariadbCR(suspend bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":       map[string]interface{}{"suspend": suspend},
	}}
}

func fakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.MariaDBGVR: "MariaDBList"},
		objs...,
	)
}

// TestSuspendResumeCR proves the fence's suspend/resume primitive against a fake
// API server — the operation the repair CR-leak fix (#1) and bootstrap rescue (#5)
// rely on. It also exercises the injectable-client seam end to end.
func TestSuspendResumeCR(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDynamic(mariadbCR(false))
	defer k8s.SetClientsForTest(&k8s.Clients{Dynamic: dyn})()
	p := testProvider()

	suspend := func() bool {
		got, err := dyn.Resource(k8s.MariaDBGVR).Namespace("ns").Get(ctx, "c", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get CR: %v", err)
		}
		return k8s.GetNestedBool(got, "spec", "suspend")
	}

	if err := p.SuspendCR(ctx); err != nil {
		t.Fatalf("SuspendCR: %v", err)
	}
	if !suspend() {
		t.Fatal("after SuspendCR, spec.suspend should be true")
	}
	if err := p.ResumeCR(ctx); err != nil {
		t.Fatalf("ResumeCR: %v", err)
	}
	if suspend() {
		t.Fatal("after ResumeCR, spec.suspend should be false")
	}
}

func recoveryPod(name string, recovery bool) *corev1.Pod {
	labels := map[string]string{"app.kubernetes.io/instance": "c"}
	if recovery {
		labels["k8s.mariadb.com/recovery"] = "true"
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels}}
}

// TestDeleteRecoveryPods proves the fence clears operator recovery pods (the #6
// ordering fix depends on this) and leaves the real cluster pods untouched.
func TestDeleteRecoveryPods(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(
		recoveryPod("c-recovery-0", true),
		recoveryPod("c-recovery-1", true),
		recoveryPod("c-0", false), // a normal cluster pod — must survive
	)
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs})()

	testProvider().DeleteRecoveryPods(ctx)

	remaining, err := cs.CoreV1().Pods("ns").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 1 || remaining.Items[0].Name != "c-0" {
		var names []string
		for _, p := range remaining.Items {
			names = append(names, p.Name)
		}
		t.Fatalf("expected only the non-recovery pod c-0 to remain, got %v", names)
	}
}

// TestWaitPodsTerminated_AllGone proves the data-safety gate returns cleanly the
// moment no cluster pods remain (the success path before wsrep_recover).
func TestWaitPodsTerminated_AllGone(t *testing.T) {
	ctx := context.Background()
	// Only an unrelated pod exists; none match the cluster selector.
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns", Labels: map[string]string{"app": "unrelated"}},
	})
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs})()

	if err := testProvider().WaitPodsTerminated(ctx, 5); err != nil {
		t.Fatalf("WaitPodsTerminated should return nil when no cluster pods remain, got %v", err)
	}
}
