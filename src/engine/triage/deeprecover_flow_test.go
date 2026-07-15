package triage

import (
	"context"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestDeepRecover_FenceRecoverRestore is a full-sequence flow test of triage's
// belly-up escalation against a fake API server. It proves the invariant that
// makes the escalation safe: deepRecover FENCES (suspend CR, scale to 0), reads
// each node's authoritative position via wsrep_recover, then RESTORES on the way
// out (scale back up, resume CR) — and never declares authority.
func TestDeepRecover_FenceRecoverRestore(t *testing.T) {
	ctx := context.Background()
	const uuid = "57a2c75c-6dea-11f1-b59b-266ed2cb955a"

	// MariaDB CR: not suspended, image set (RunWsrepRecover needs spec.image).
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec": map[string]interface{}{
			"suspend":  false,
			"replicas": int64(3),
			"image":    "mariadb:11",
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.MariaDBGVR: "MariaDBList"},
		cr,
	)

	// A StatefulSet at 3 replicas; NO cluster pods (belly-up, already down).
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
	}
	cs := fake.NewSimpleClientset(sts)

	// Auto-succeed any helper pod the moment it's created, so RunWsrepRecover's
	// phase poll returns instead of timing out against a pod the fake won't run.
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = corev1.PodSucceeded
		}
		return false, nil, nil
	})
	// The fake clientset's StatefulSet scale subresource panics, so back GetScale/
	// UpdateScale with reactors: serve the current replica count, record every
	// target, so we can prove the fence (0) → restore (3) order.
	current := int32(3)
	var scaleTo []int32
	cs.PrependReactor("get", "statefulsets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga := a.(clienttesting.GetAction)
		if ga.GetSubresource() != "scale" {
			return false, nil, nil
		}
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace()},
			Spec:       autoscalingv1.ScaleSpec{Replicas: current},
		}, nil
	})
	cs.PrependReactor("update", "statefulsets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		ua := a.(clienttesting.UpdateAction)
		if ua.GetSubresource() != "scale" {
			return false, nil, nil
		}
		sc := ua.GetObject().(*autoscalingv1.Scale)
		current = sc.Spec.Replicas
		scaleTo = append(scaleTo, sc.Spec.Replicas)
		return true, sc, nil
	})

	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()
	// Canned wsrep_recover output so ParseWsrepRecoverOutput yields a real position.
	defer k8s.SetPodLogsHookForTest(func(ctx context.Context, ns, pod string) string {
		return "WSREP: Recovered position: " + uuid + ":552481\nWSREP: Last committed: 552481\n"
	})()

	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns", DeleteTimeout: 5}, 3, cr)
	tr := &galeraTriage{p: p}
	data := &galeraTriageData{
		grastateData:   []grastate{{Pod: "c-0"}},
		wsrepRecovered: make(map[string]provider.WsrepRecoverResult),
	}

	tr.deepRecover(ctx, data)

	// 1) Authoritative position was read and stored (the point of the escalation).
	rr, ok := data.wsrepRecovered["c-0"]
	if !ok || !rr.Valid {
		t.Fatalf("wsrepRecovered[c-0] should hold a valid position, got %+v (ok=%v)", rr, ok)
	}
	if rr.UUID != uuid || rr.Seqno != 552481 {
		t.Fatalf("recovered position: got uuid=%s seqno=%d, want %s:552481", rr.UUID, rr.Seqno, uuid)
	}

	// 2) Fence then restore: scaled to 0, then back to 3.
	if len(scaleTo) != 2 || scaleTo[0] != 0 || scaleTo[1] != 3 {
		t.Fatalf("scale sequence: got %v, want [0 3] (fence to 0, restore to 3)", scaleTo)
	}

	// 3) CR resumed on the way out (the always-restore guarantee).
	got, err := dyn.Resource(k8s.MariaDBGVR).Namespace("ns").Get(ctx, "c", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR: %v", err)
	}
	if k8s.GetNestedBool(got, "spec", "suspend") {
		t.Fatal("CR left suspended — deepRecover must resume on exit")
	}

	// 4) Non-destructive: no authority was declared.
	if k8s.GetNestedString(got, "spec", "galera", "recovery", "forceClusterBootstrapInPod") != "" {
		t.Fatal("deepRecover must NOT set forceClusterBootstrapInPod — evaluation only")
	}
}
