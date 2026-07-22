package repair

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func donorRepair(t *testing.T, force bool) *galeraRepair {
	t.Helper()
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1", "kind": "MariaDB",
		"metadata": map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":     map[string]interface{}{"replicas": int64(3)},
	}}
	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns", Force: force}, 3, cr)
	return &galeraRepair{p: p}
}

func syncedCandidate(pod string, ord int) model.InstanceAssessment {
	return model.InstanceAssessment{Pod: pod, Instance: ord, IsRunning: true,
		WsrepReady: "ON", WsrepConnected: "ON", WsrepStateComment: "Synced"}
}

// TestResolveAutoDonor_AmbiguousRefuses is the #1 donor guard: when authority is
// ambiguous (split-brain / divergent lineage → SafeToHeal=false), auto-donor
// selection MUST refuse — even when Synced candidates exist, even with --force —
// forcing the operator to declare the source explicitly. A wrong auto-donor clones
// divergent data over a good node (silent data loss). These paths return before any
// wsrep probe, so they are pure.
func TestResolveAutoDonor_AmbiguousRefuses(t *testing.T) {
	result := &model.TriageResult{
		DataComparison: model.DataComparison{SafeToHeal: false}, // ambiguous
		Assessments:    []model.InstanceAssessment{syncedCandidate("c-0", 0), syncedCandidate("c-1", 1)},
	}
	for _, force := range []bool{false, true} {
		g := donorRepair(t, force)
		if _, err := g.resolveAutoDonor(context.Background(), result); err == nil {
			t.Fatalf("force=%v: ambiguous authority must abort auto-donor selection", force)
		} else if !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("force=%v: want an ambiguity abort, got %v", force, err)
		}
	}
}

// Ambiguous AND no healthy candidate → still aborts, telling the operator to declare
// the source.
func TestResolveAutoDonor_AmbiguousNoCandidates(t *testing.T) {
	g := donorRepair(t, false)
	result := &model.TriageResult{
		DataComparison: model.DataComparison{SafeToHeal: false},
		Assessments:    []model.InstanceAssessment{{Pod: "c-0", IsRunning: true, WsrepStateComment: "Initialized"}},
	}
	_, err := g.resolveAutoDonor(context.Background(), result)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want an ambiguity abort, got %v", err)
	}
}

// Unambiguous but nothing Synced → abort with the no-healthy-donor message (never a
// silent "heal anyway").
func TestResolveAutoDonor_UnambiguousNoCandidates(t *testing.T) {
	g := donorRepair(t, false)
	result := &model.TriageResult{
		DataComparison: model.DataComparison{SafeToHeal: true},
		Assessments:    []model.InstanceAssessment{{Pod: "c-0", IsRunning: true, WsrepStateComment: "Initialized"}},
	}
	_, err := g.resolveAutoDonor(context.Background(), result)
	if err == nil || !strings.Contains(err.Error(), "No healthy donor") {
		t.Fatalf("want no-healthy-donor abort, got %v", err)
	}
}

// A structurally invalid explicit ordinal aborts before any cluster call — even with
// --force (explicit intent does not bypass structural validation).
func TestResolveExplicitDonor_OrdinalOutOfRange(t *testing.T) {
	g := donorRepair(t, true)
	_, err := g.resolveExplicitDonor(context.Background(), 5, &model.TriageResult{})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want out-of-range abort, got %v", err)
	}
}
