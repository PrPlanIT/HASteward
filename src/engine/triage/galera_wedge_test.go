package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// wedgeCR builds a MariaDB CR with a GaleraReady condition, optional spec.suspend, and an
// optional status.galeraRecovery.state (pod -> seqno) — the real shapes observed on the
// live kimai-mariadb wedge.
func wedgeCR(galeraReadyStatus string, suspend bool, recoveryState map[string]int64) *unstructured.Unstructured {
	status := map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type": "GaleraReady", "status": galeraReadyStatus,
				"reason": "GaleraNotReady", "message": "Galera not ready",
			},
		},
	}
	if recoveryState != nil {
		state := map[string]interface{}{}
		for pod, seqno := range recoveryState {
			state[pod] = map[string]interface{}{
				"seqno": seqno, "uuid": "64424f11-3e6d-11f1-8885-1e8e8ef64a07", "safeToBootstrap": false,
			}
		}
		status["galeraRecovery"] = map[string]interface{}{"state": state}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":       map[string]interface{}{"suspend": suspend, "replicas": int64(3), "image": "mariadb:11"},
		"status":     status,
	}}
}

func detectWedge(cr *unstructured.Unstructured, data *galeraTriageData, cmp model.DataComparison, as []model.InstanceAssessment) *model.OperatorWedge {
	p := provider.NewGaleraProviderForTest(&common.Config{ClusterName: "c", Namespace: "ns"}, 3, cr)
	return (&galeraTriage{p: p}).detectOperatorWedge(data, cmp, as)
}

// healthyData / healthyCmp / healthyAssessments describe a data plane with nothing wrong —
// the basis on which a control-plane wedge is the only remaining explanation for the flap.
func healthyData() *galeraTriageData {
	return &galeraTriageData{allNodesDown: false, bestSeqnoNode: "c-0"}
}
func healthyCmp() model.DataComparison { return model.DataComparison{SafeToHeal: true} }
func healthyAssessments() []model.InstanceAssessment {
	return []model.InstanceAssessment{{Pod: "c-0"}, {Pod: "c-1"}, {Pod: "c-2"}}
}

// stuckRecovery is the live signature: every node's seqno unresolved (-1).
var stuckRecovery = map[string]int64{"c-0": -1, "c-1": -1, "c-2": -1}

// TestDetectOperatorWedge_FiresOnLiveSignature reproduces the kimai case: healthy data,
// GaleraReady:False, a galeraRecovery snapshot with all seqno -1, cluster suspended.
func TestDetectOperatorWedge_FiresOnLiveSignature(t *testing.T) {
	w := detectWedge(wedgeCR("False", true, stuckRecovery), healthyData(), healthyCmp(), healthyAssessments())
	if w == nil {
		t.Fatal("expected an operator wedge to be detected (data healthy + GaleraReady:False + stuck recovery)")
	}
	if !w.Suspended {
		t.Error("wedge should be marked Suspended (latent) — spec.suspend was true")
	}
	if len(w.RecoveryNodes) != 3 {
		t.Errorf("expected all 3 unresolved nodes, got %v", w.RecoveryNodes)
	}
	if w.RecoveryNodes[0] != "c-0" {
		t.Errorf("recovery nodes should be sorted, got %v", w.RecoveryNodes)
	}
	if w.BestCandidate != "c-0" {
		t.Errorf("expected BestCandidate c-0 (the unstick target), got %q", w.BestCandidate)
	}
	if w.Reason != "GaleraNotReady" || w.Message != "Galera not ready" {
		t.Errorf("condition reason/message not captured: %q / %q", w.Reason, w.Message)
	}
}

// TestDetectOperatorWedge_NegativeCases: each leg of the contradiction is necessary.
func TestDetectOperatorWedge_NegativeCases(t *testing.T) {
	tests := []struct {
		name string
		cr   *unstructured.Unstructured
		data *galeraTriageData
		cmp  model.DataComparison
		as   []model.InstanceAssessment
	}{
		{
			name: "operator agrees cluster is healthy (GaleraReady:True)",
			cr:   wedgeCR("True", false, stuckRecovery), data: healthyData(), cmp: healthyCmp(), as: healthyAssessments(),
		},
		{
			name: "no recovery snapshot at all",
			cr:   wedgeCR("False", false, nil), data: healthyData(), cmp: healthyCmp(), as: healthyAssessments(),
		},
		{
			name: "a node resolved a valid seqno (operator can bootstrap — not wedged)",
			cr:   wedgeCR("False", false, map[string]int64{"c-0": 1655026, "c-1": -1, "c-2": -1}),
			data: healthyData(), cmp: healthyCmp(), as: healthyAssessments(),
		},
		{
			name: "data plane has real work (a node needs heal)",
			cr:   wedgeCR("False", false, stuckRecovery), data: healthyData(), cmp: healthyCmp(),
			as: []model.InstanceAssessment{{Pod: "c-0"}, {Pod: "c-1", NeedsHeal: true}, {Pod: "c-2"}},
		},
		{
			name: "authority ambiguous (not safe to heal)",
			cr:   wedgeCR("False", false, stuckRecovery), data: healthyData(),
			cmp: model.DataComparison{SafeToHeal: false}, as: healthyAssessments(),
		},
		{
			name: "all nodes down (a real cluster-down, not a control-plane wedge)",
			cr:   wedgeCR("False", false, stuckRecovery),
			data: &galeraTriageData{allNodesDown: true, bestSeqnoNode: "c-0"}, cmp: healthyCmp(), as: healthyAssessments(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := detectWedge(tc.cr, tc.data, tc.cmp, tc.as); w != nil {
				t.Fatalf("expected NO wedge, got %+v", w)
			}
		})
	}
}
