package repair

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func guardCluster() *unstructured.Unstructured {
	// pg-3 is the (stale) primary; pg-2 is the data authority (leader_not_primary).
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "pg", "namespace": "ns"},
		"spec":       map[string]interface{}{"instances": int64(3)},
		"status":     map[string]interface{}{"currentPrimary": "pg-3"},
	}}
}

// TestPlanTargeted_RefusesToHealAuthorityEvenWithForce is the P3.2 action-layer guard:
// `repair --instance <authority> --force` must REFUSE — healing rm -rf's the target and
// re-clones from the stale primary, which would destroy the newest data. --force cannot
// override this, and no destructive step (PVC lookup / helper pod) may be reached.
func TestPlanTargeted_RefusesToHealAuthorityEvenWithForce(t *testing.T) {
	ctx := context.Background()
	cluster := guardCluster()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)
	var pvcFetched bool
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		pvcFetched = true
		return false, nil, nil
	})
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	inst := 2 // pg-2, the authority
	p := provider.NewCNPGProviderForTest(
		&common.Config{ClusterName: "pg", Namespace: "ns", InstanceNumber: &inst, Force: true}, 3, cluster)
	r := &cnpgRepair{p: p}

	result := &model.TriageResult{
		DataComparison: model.DataComparison{
			SafeToHeal:   false,
			Authority:    model.AuthorityLeaderNotPrimary,
			MostAdvanced: "pg-2",
		},
	}

	_, err := r.planTargeted(ctx, result)
	if err == nil || !strings.Contains(err.Error(), "DATA AUTHORITY") {
		t.Fatalf("must refuse to heal the authority even with --force, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--force cannot override") {
		t.Fatalf("refusal must make clear --force cannot override it, got: %v", err)
	}
	if pvcFetched {
		t.Fatal("guard must fire BEFORE any destructive step — no PVC lookup should have happened")
	}
}

// TestPlanTargeted_AllowsForcedHealOfNonAuthority proves the guard is surgical: under
// the same unsafe/split-brain state, a --force targeted heal of a NON-authority replica
// still proceeds (that is exactly what --force is for) and is not swept up by the guard.
func TestPlanTargeted_AllowsForcedHealOfNonAuthority(t *testing.T) {
	ctx := context.Background()
	cluster := guardCluster()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.CNPGClusterGVR: "ClusterList"},
		cluster,
	)
	// pg-1 is a disposable replica with a PVC present.
	cs := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-1", Namespace: "ns"},
	})
	defer k8s.SetClientsForTest(&k8s.Clients{Clientset: cs, Dynamic: dyn})()

	inst := 1 // pg-1, NOT the authority (authority is pg-2)
	p := provider.NewCNPGProviderForTest(
		&common.Config{ClusterName: "pg", Namespace: "ns", InstanceNumber: &inst, Force: true}, 3, cluster)
	r := &cnpgRepair{p: p}

	result := &model.TriageResult{
		DataComparison: model.DataComparison{SafeToHeal: false, Authority: model.AuthorityLeaderNotPrimary, MostAdvanced: "pg-2"},
		Assessments:    []model.InstanceAssessment{{Pod: "pg-1", Instance: 1, NeedsHeal: true, Notes: []string{"diverged"}}},
	}

	targets, err := r.planTargeted(ctx, result)
	if err != nil {
		t.Fatalf("forced heal of a non-authority replica must proceed, got error: %v", err)
	}
	if len(targets) != 1 || targets[0].Pod != "pg-1" {
		t.Fatalf("expected a single heal target pg-1, got %+v", targets)
	}
}
