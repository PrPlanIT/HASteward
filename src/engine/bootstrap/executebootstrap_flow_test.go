package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output/model"

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

// TestExecuteBootstrap_RescueRevertsAuthorityMarkers is the #5 regression guard.
// It drives executeBootstrap through the fence + recover + authority-mutation
// steps, then injects a STEP 8 scale-up failure so rescue() runs, and asserts the
// rescue is SYMMETRIC: it reverts safe_to_bootstrap (a safeboot-revert helper) AND
// clears forceClusterBootstrapInPod, so a failed bootstrap never leaves the
// operator configured to bootstrap from an abandoned candidate. Also covers the
// #6 fence ordering (DeleteRecoveryPods before the terminate wait) by getting
// through the fence at all.
func TestExecuteBootstrap_RescueRevertsAuthorityMarkers(t *testing.T) {
	ctx := context.Background()
	const uuid = "57a2c75c-6dea-11f1-b59b-266ed2cb955a"

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

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(sts)

	// Auto-succeed helper pods; record their names so we can prove which helpers ran.
	var createdPods []string
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = corev1.PodSucceeded
			createdPods = append(createdPods, pod.Name)
		}
		return false, nil, nil
	})
	// Scale via reactors (fake scale subresource panics). Fence to 0 succeeds; the
	// STEP 8 restore to 3 is INJECTED to fail, forcing rescue().
	current := int32(3)
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
		if sc.Spec.Replicas != 0 {
			return true, nil, fmt.Errorf("injected scale-up failure") // STEP 8
		}
		current = sc.Spec.Replicas
		return true, sc, nil
	})

	defer common.DisableSleepForTest()() // run the helper-pod poll loops instantly
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()
	defer k8s.SetPodLogsHookForTest(func(ctx context.Context, ns, pod string) string {
		return "WSREP: Recovered position: " + uuid + ":552481\nWSREP: Last committed: 552481\n"
	})()

	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns", DeleteTimeout: 5}, 3, cr)
	b := &galeraBootstrap{p: p}

	// One assessment = the candidate, so STEP 4b (gcache) and STEP 5 (clear peers)
	// both skip, keeping the helper-pod count (and test time) down.
	assessments := []model.InstanceAssessment{
		{Pod: "c-0", Instance: 0, UUID: uuid, EffectiveSeqno: 552481, SafeToBootstrap: "0"},
	}
	err := b.executeBootstrap(ctx, "c-0", true /*bellyUp*/, assessments, &model.BootstrapResult{})

	// The run must fail (STEP 8 scale-up was injected to fail).
	if err == nil {
		t.Fatal("expected executeBootstrap to fail on the injected scale-up error")
	}

	got, gerr := dyn.Resource(k8s.MariaDBGVR).Namespace("ns").Get(ctx, "c", metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("get CR: %v", gerr)
	}

	// #5: forceClusterBootstrapInPod (set in STEP 7) must be CLEARED by rescue —
	// otherwise the operator would bootstrap from the failed candidate.
	if v := k8s.GetNestedString(got, "spec", "galera", "recovery", "forceClusterBootstrapInPod"); v != "" {
		t.Fatalf("rescue left forceClusterBootstrapInPod=%q — must be cleared on failure", v)
	}

	// #5: rescue must have reverted safe_to_bootstrap via a revert helper pod.
	var sawRevert bool
	for _, n := range createdPods {
		if strings.Contains(n, "safeboot-revert") {
			sawRevert = true
		}
	}
	if !sawRevert {
		t.Fatalf("rescue did not run a safeboot-revert helper; created pods: %v", createdPods)
	}

	// The CR must be resumed on the way out.
	if k8s.GetNestedBool(got, "spec", "suspend") {
		t.Fatal("CR left suspended after a failed bootstrap — rescue must resume it")
	}
}
