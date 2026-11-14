package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output/model"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// onlineCR builds a MariaDB CR whose GaleraReady condition is already `status` — with
// "True", waitGaleraReady returns on the first poll (the operator "reformed").
func onlineCR(galeraReady string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":       map[string]interface{}{"suspend": true, "replicas": int64(3)},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "GaleraReady", "status": galeraReady},
			},
		},
	}}
}

func onlineSetup(cr *unstructured.Unstructured) (*galeraBootstrap, *dynamicfake.FakeDynamicClient, func()) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.MariaDBGVR: "MariaDBList"},
		cr,
	)
	restoreSleep := common.DisableSleepForTest()
	restoreClients := k8s.SetClientsForTest(&k8s.Clients{Clientset: fake.NewSimpleClientset(), Dynamic: dyn})
	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns"}, 3, cr)
	return &galeraBootstrap{p: p}, dyn, func() { restoreClients(); restoreSleep() }
}

var deadlockDiagnosis = model.Diagnosis{ID: "galera-operator-recovery-deadlock", Target: "c-0", Summary: "stuck"}

// TestBootstrapOnline_ForceBootstrapsAndClears drives the execute path: it must patch
// forceClusterBootstrapInPod on the LIVE cluster, wait for GaleraReady, and defensively
// clear the force patch and the operator's recovery snapshot afterward.
func TestBootstrapOnline_ForceBootstrapsAndClears(t *testing.T) {
	b, dyn, cleanup := onlineSetup(onlineCR("True"))
	defer cleanup()

	var forcePatched, forceCleared, recoveryCleared, scaled bool
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		body := string(a.(clienttesting.PatchAction).GetPatch())
		switch {
		case a.GetSubresource() == "status" && strings.Contains(body, `"galeraRecovery":null`):
			recoveryCleared = true
		case strings.Contains(body, "forceClusterBootstrapInPod") && strings.Contains(body, "c-0"):
			forcePatched = true
		case strings.Contains(body, "remove") && strings.Contains(body, "forceClusterBootstrapInPod"):
			forceCleared = true
		}
		return false, nil, nil
	})

	res, err := b.bootstrapOnline(context.Background(), deadlockDiagnosis, false, &model.BootstrapResult{}, nil)
	if err != nil {
		t.Fatalf("online bootstrap failed: %v", err)
	}
	if !forcePatched {
		t.Fatal("must patch forceClusterBootstrapInPod=c-0 on the live cluster")
	}
	if !forceCleared {
		t.Fatal("must defensively clear forceClusterBootstrapInPod after GaleraReady")
	}
	if !recoveryCleared {
		t.Fatal("must clear status.galeraRecovery after GaleraReady")
	}
	if scaled {
		t.Fatal("online path must NOT scale the live cluster")
	}
	if res.Decision.CandidatePod != "c-0" {
		t.Errorf("decision should name the bootstrap source c-0, got %q", res.Decision.CandidatePod)
	}
}

// TestBootstrapOnline_DryRunNoMutation: --dry-run must patch nothing.
func TestBootstrapOnline_DryRunNoMutation(t *testing.T) {
	b, dyn, cleanup := onlineSetup(onlineCR("True"))
	defer cleanup()

	var patched bool
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})

	res, err := b.bootstrapOnline(context.Background(), deadlockDiagnosis, true, &model.BootstrapResult{}, nil)
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if patched {
		t.Fatal("dry-run must not patch the cluster (#27 remedy is preview-safe)")
	}
	if len(res.ActionsPlanned) == 0 {
		t.Fatal("dry-run should still return the planned actions")
	}
}

// TestBootstrapOnline_DeclaredSourcePlansEscrowFirst: a declared source's plan says so,
// is marked forced, and escrows every node before the first mutation.
func TestBootstrapOnline_DeclaredSourcePlansEscrowFirst(t *testing.T) {
	b, _, cleanup := onlineSetup(onlineCR("True"))
	defer cleanup()

	as := []model.InstanceAssessment{{Pod: "c-0"}, {Pod: "c-1"}, {Pod: "c-2"}}
	d := model.Diagnosis{ID: "operator-declared-source", Target: "c-1"}
	res, err := b.bootstrapOnline(context.Background(), d, true, &model.BootstrapResult{}, as)
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if !strings.Contains(res.Decision.Reason, "declared") || !res.Decision.ForceRequired {
		t.Fatalf("decision must report the declared, forced source; got %+v", res.Decision)
	}
	if len(res.ActionsPlanned) == 0 || res.ActionsPlanned[0].Phase != model.PhaseEscrow {
		t.Fatalf("escrow must be the first planned action; got %+v", res.ActionsPlanned)
	}
}

// TestBootstrapOnline_EmptyTargetRefuses: no bootstrap source → refuse, patch nothing.
func TestBootstrapOnline_EmptyTargetRefuses(t *testing.T) {
	b, dyn, cleanup := onlineSetup(onlineCR("True"))
	defer cleanup()
	var patched bool
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})

	_, err := b.bootstrapOnline(context.Background(),
		model.Diagnosis{ID: "galera-operator-recovery-deadlock"}, false, &model.BootstrapResult{}, nil)
	if err == nil {
		t.Fatal("must refuse when the diagnosis names no bootstrap source")
	}
	if patched {
		t.Fatal("must not patch anything when refusing")
	}
}

// TestBootstrapOnline_TimesOutIfNeverReady: if the operator never reaches GaleraReady, the
// remedy reports failure rather than claiming success.
func TestBootstrapOnline_TimesOutIfNeverReady(t *testing.T) {
	b, dyn, cleanup := onlineSetup(onlineCR("False")) // never becomes Ready
	defer cleanup()

	var forceCleared bool
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		body := string(a.(clienttesting.PatchAction).GetPatch())
		if strings.Contains(body, "remove") && strings.Contains(body, "forceClusterBootstrapInPod") {
			forceCleared = true
		}
		return false, nil, nil
	})

	_, err := b.bootstrapOnline(context.Background(), deadlockDiagnosis, false, &model.BootstrapResult{}, nil)
	if err == nil || !strings.Contains(err.Error(), "GaleraReady") {
		t.Fatalf("must fail when the cluster never becomes GaleraReady, got: %v", err)
	}
	if !forceCleared {
		t.Fatal("a failed online bootstrap must clear forceClusterBootstrapInPod, or the operator retries it forever")
	}
}

// A declared source takes the online remedy only when triage proved the live group's
// authority; a live group it can't prove (e.g. on the zero UUID) can't take joiners.
func TestOnlineDeclaredOK(t *testing.T) {
	if onlineDeclaredOK(&model.TriageResult{DataComparison: model.DataComparison{SafeToHeal: false}}) {
		t.Fatal("an unprovable live group was sent down the online path")
	}
	if !onlineDeclaredOK(&model.TriageResult{DataComparison: model.DataComparison{SafeToHeal: true}}) {
		t.Fatal("control: a provable live group should take the online path")
	}
}
