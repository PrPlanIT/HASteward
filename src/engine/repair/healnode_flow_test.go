package repair

import (
	"context"
	"strings"
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

// TestHealNode_NeverReadyReturnsError is the #7 regression guard. healNode drives
// the full suspend -> scale-down -> wipe -> scale-up -> resume sequence for the
// highest ordinal, then waits for the pod to come back Ready. Here the fake never
// recreates it, so the wait exhausts — and healNode must return an ERROR (not nil),
// so the orchestrator never records an un-rejoined node as healed. It also proves
// the CR is resumed and the StatefulSet is scaled back before that failure.
func TestHealNode_NeverReadyReturnsError(t *testing.T) {
	ctx := context.Background()

	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":       map[string]interface{}{"suspend": false, "replicas": int64(3), "image": "mariadb:11"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.MariaDBGVR: "MariaDBList"},
		cr,
	)

	// StatefulSet with a real selector, and one OTHER running pod (c-0) so the
	// bootstrap-vs-repair guard sees a join target. The target c-2 is NOT created,
	// so the pod-gone wait is instant and the pod never comes back Ready.
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/instance": "c"}},
		},
	}
	c0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "c-0", Namespace: "ns", Labels: map[string]string{"app.kubernetes.io/instance": "c"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewSimpleClientset(sts, c0)

	var createdPods []string
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = corev1.PodSucceeded
			createdPods = append(createdPods, pod.Name)
		}
		return false, nil, nil
	})
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

	defer common.DisableSleepForTest()()
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	p := provider.NewGaleraProviderForTest(&common.Config{
		ClusterName: "c", Namespace: "ns", DeleteTimeout: 5, HealTimeout: 10,
	}, 3, cr)
	g := &galeraRepair{p: p}

	err := g.healNode(ctx, "c-2", 2) // highest ordinal (3-1)

	// #7: a node that never comes Ready must be an ERROR, not a silent success.
	if err == nil {
		t.Fatal("healNode returned nil for a node that never became Ready — must return an error")
	}
	if !strings.Contains(err.Error(), "did not become Ready") {
		t.Fatalf("unexpected error (want a not-Ready failure): %v", err)
	}

	// The flow ran: scaled down to the ordinal, then back up.
	if len(scaleTo) != 2 || scaleTo[0] != 2 || scaleTo[1] != 3 {
		t.Fatalf("scale sequence: got %v, want [2 3]", scaleTo)
	}
	// A storage wipe helper was created.
	var sawWipe bool
	for _, n := range createdPods {
		if strings.Contains(n, "heal-storage") {
			sawWipe = true
		}
	}
	if !sawWipe {
		t.Fatalf("no storage wipe helper was created; created pods: %v", createdPods)
	}
	// CR resumed before the failure (STEP 5 resumes; the deferred Cleanup also guards).
	got, gerr := dyn.Resource(k8s.MariaDBGVR).Namespace("ns").Get(ctx, "c", metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("get CR: %v", gerr)
	}
	if k8s.GetNestedBool(got, "spec", "suspend") {
		t.Fatal("CR left suspended after healNode — must be resumed")
	}
}
