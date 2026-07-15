package repair

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestGaleraCleanup_ResumesWhenSuspended guards #29: a repair that suspended the operator
// CR must resume it on cleanup — the always-restore backstop — even when handed an already
// cancelled context (an interrupted run). Leaving the operator suspended is the worst
// outcome. (The fake client does not enforce ctx cancellation, so this guards the resume
// CONTRACT; the fresh-context robustness in Cleanup is by construction.)
func TestGaleraCleanup_ResumesWhenSuspended(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":       map[string]interface{}{"suspend": true, "replicas": int64(3)},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.MariaDBGVR: "MariaDBList"},
		cr,
	)
	var resumePatched bool
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pa, ok := a.(clienttesting.PatchAction); ok && strings.Contains(string(pa.GetPatch()), `"suspend":false`) {
			resumePatched = true
		}
		return false, nil, nil
	})
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: fake.NewSimpleClientset(), Dynamic: dyn})()

	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns"}, 3, cr)
	g := &galeraRepair{p: p, crSuspended: true}

	// Simulate an interrupted run: the context handed to Cleanup is already cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.Cleanup(ctx)

	if !resumePatched {
		t.Fatal("Cleanup must resume the CR (patch suspend:false) when it holds it suspended (#29)")
	}
	if g.crSuspended {
		t.Fatal("crSuspended should be cleared after a successful resume")
	}
}

// TestGaleraCleanup_NoopWhenNotSuspended: cleanup must not patch when it never suspended.
func TestGaleraCleanup_NoopWhenNotSuspended(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.MariaDBGVR: "MariaDBList"},
	)
	var patched bool
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: fake.NewSimpleClientset(), Dynamic: dyn})()

	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns"}, 3, nil)
	g := &galeraRepair{p: p, crSuspended: false}
	g.Cleanup(context.Background())

	if patched {
		t.Fatal("Cleanup must be a no-op when it never suspended the CR")
	}
}
