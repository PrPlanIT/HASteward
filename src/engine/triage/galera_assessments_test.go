package triage

import (
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func galeraTriageForTest() *galeraTriage {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1", "kind": "MariaDB",
		"metadata": map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":     map[string]interface{}{"replicas": int64(3)},
	}}
	return &galeraTriage{p: provider.NewGaleraProviderForTest(
		&common.Config{ClusterName: "c", Namespace: "ns"}, 3, cr)}
}

func runningPod(name string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func findAssessment(as []model.InstanceAssessment, pod string) *model.InstanceAssessment {
	for i := range as {
		if as[i].Pod == pod {
			return &as[i]
		}
	}
	return nil
}

func notesContain(a *model.InstanceAssessment, sub string) bool {
	for _, n := range a.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestGaleraBuildAssessments_UnreadDoesNotAutoHeal pins the P2.1 fix: a node whose
// wsrep query FAILED after retries (Unread sentinel) must NOT be classified as a
// non-primary node needing a wipe+SST. A transient read failure never drives a
// destructive heal.
func TestGaleraBuildAssessments_UnreadDoesNotAutoHeal(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData: []grastate{{Pod: "c-0", Source: "exec"}},
		runningPods:  []corev1.Pod{runningPod("c-0")},
		wsrepMap:     map[string]*wsrepStatus{"c-0": {LastCommitted: -1, Unread: true}},
	}
	as := tr.buildAssessments(data, &model.DataComparison{SafeToHeal: true})
	a := findAssessment(as, "c-0")
	if a == nil {
		t.Fatal("no assessment for c-0")
	}
	if a.NeedsHeal {
		t.Fatalf("an UNREAD node must not be auto-healed; notes=%v", a.Notes)
	}
	if !notesContain(a, "UNREAD") {
		t.Fatalf("want an UNREAD note, got %v", a.Notes)
	}
}

// A genuinely disconnected node (connected=OFF from a real reading) DOES need heal —
// the fix must not suppress real failures.
func TestGaleraBuildAssessments_DisconnectedNeedsHeal(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData: []grastate{{Pod: "c-0", Source: "exec"}},
		runningPods:  []corev1.Pod{runningPod("c-0")},
		wsrepMap:     map[string]*wsrepStatus{"c-0": {Connected: "OFF", Ready: "OFF", ClusterStatus: "non-Primary"}},
	}
	as := tr.buildAssessments(data, &model.DataComparison{SafeToHeal: true})
	a := findAssessment(as, "c-0")
	if a == nil || !a.NeedsHeal {
		t.Fatalf("a disconnected node needs heal; got %+v", a)
	}
}

// A Synced, connected, ready, Primary-component node needs no action.
func TestGaleraBuildAssessments_SyncedHealthy(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData:   []grastate{{Pod: "c-0", Source: "exec"}},
		runningPods:    []corev1.Pod{runningPod("c-0")},
		primaryMembers: []string{"c-0"},
		wsrepMap: map[string]*wsrepStatus{"c-0": {
			LocalState: 4, LocalStateComment: "Synced", Connected: "ON", Ready: "ON", ClusterStatus: "Primary",
		}},
	}
	as := tr.buildAssessments(data, &model.DataComparison{SafeToHeal: true})
	a := findAssessment(as, "c-0")
	if a == nil || a.NeedsHeal {
		t.Fatalf("a Synced healthy node must not need heal; got %+v", a)
	}
}

// The data-loss guard: under ambiguous authority (!SafeToHeal), a node AHEAD of the
// primary component is flagged for manual review, NOT auto-healed.
func TestGaleraBuildAssessments_AheadOfPrimaryUnderSplitBrain(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData:    []grastate{{Pod: "c-1", Source: "exec"}},
		runningPods:     []corev1.Pod{runningPod("c-1")},
		effectiveSeqnos: map[string]*effectiveSeqno{"c-1": {Value: 100, Known: true, Source: "grastate"}},
		// c-1 is NOT in the primary component.
	}
	cmp := &model.DataComparison{SafeToHeal: false, BestPrimarySeqno: 50}
	as := tr.buildAssessments(data, cmp)
	a := findAssessment(as, "c-1")
	if a == nil {
		t.Fatal("no assessment for c-1")
	}
	if a.NeedsHeal {
		t.Fatalf("a node ahead of the primary under split-brain must NOT be auto-healed; notes=%v", a.Notes)
	}
	if !notesContain(a, "AHEAD OF PRIMARY") {
		t.Fatalf("want an AHEAD-OF-PRIMARY note, got %v", a.Notes)
	}
}
