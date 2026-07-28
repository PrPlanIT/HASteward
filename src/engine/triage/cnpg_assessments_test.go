package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func cnpgTriageForTest() *cnpgTriage {
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]interface{}{"name": "pg", "namespace": "ns"},
		"spec":     map[string]interface{}{"instances": int64(3)},
	}}
	return &cnpgTriage{p: provider.NewCNPGProviderForTest(
		&common.Config{ClusterName: "pg", Namespace: "ns"}, 3, cluster)}
}

// buildCNPGData wires a primary (pg-1, timeline 9, LSN 50/0) plus one replica under
// test, with SafeToHeal true and the primary as authority.
func buildCNPGData(replica controlData, streaming, crashloop []string) (*cnpgTriageData, *model.DataComparison) {
	primary := controlData{Pod: "pg-1", Timeline: "9", CheckpointLocation: "50/00000000", Source: "exec"}
	data := &cnpgTriageData{
		controlData:       []controlData{primary, replica},
		primaryTimeline:   "9",
		streamingReplicas: streaming,
	}
	data.primaryControlData = &data.controlData[0]
	for _, c := range crashloop {
		data.crashloopPods = append(data.crashloopPods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: c}})
	}
	return data, &model.DataComparison{SafeToHeal: true, MostAdvanced: "pg-1"}
}

func cnpgAssess(t *testing.T, replica controlData, streaming, crashloop []string) *model.InstanceAssessment {
	t.Helper()
	tr := cnpgTriageForTest()
	data, cmp := buildCNPGData(replica, streaming, crashloop)
	as := tr.buildAssessments(data, cmp, "pg-1")
	a := findAssessment(as, replica.Pod)
	if a == nil {
		t.Fatalf("no assessment for %s", replica.Pod)
	}
	return a
}

// TestCnpgBuildAssessments_RunningReadyFlags guards the fix for the cosmetic bug where
// CNPG never populated IsRunning/IsReady (always false — a Running 1/1 primary read as
// not-running). A Running-phase pod is running; ready only when its postgres container is
// (crashloop = running-but-not-ready); a stranded instance with no pod is neither.
func TestCnpgBuildAssessments_RunningReadyFlags(t *testing.T) {
	tr := cnpgTriageForTest()
	data := &cnpgTriageData{
		controlData: []controlData{
			{Pod: "pg-1", Timeline: "9", CheckpointLocation: "50/00000000", Source: "exec"},
			{Pod: "pg-2", Timeline: "9", CheckpointLocation: "40/00000000", Source: "exec"},
			{Pod: "pg-3", Timeline: "9", CheckpointLocation: "40/00000000", Source: "pvc_probe"},
		},
		primaryTimeline: "9",
		runningPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pg-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pg-2"}}, // Running phase, but crash-looping
		},
		crashloopPods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pg-2"}}},
	}
	data.primaryControlData = &data.controlData[0]
	as := tr.buildAssessments(data, &model.DataComparison{SafeToHeal: true, MostAdvanced: "pg-1"}, "pg-1")

	want := map[string][2]bool{ // pod -> {IsRunning, IsReady}
		"pg-1": {true, true},   // running + ready primary (previously reported false/false)
		"pg-2": {true, false},  // running but crash-looping → not ready
		"pg-3": {false, false}, // stranded, no pod
	}
	for pod, w := range want {
		a := findAssessment(as, pod)
		if a == nil {
			t.Fatalf("no assessment for %s", pod)
		}
		if a.IsRunning != w[0] || a.IsReady != w[1] {
			t.Errorf("%s: IsRunning=%v IsReady=%v, want %v/%v", pod, a.IsRunning, a.IsReady, w[0], w[1])
		}
	}
}

func TestCnpgBuildAssessments_PrimaryNeverHeals(t *testing.T) {
	tr := cnpgTriageForTest()
	data, cmp := buildCNPGData(controlData{Pod: "pg-2", Timeline: "9", CheckpointLocation: "50/00000000", Source: "exec"}, nil, nil)
	as := tr.buildAssessments(data, cmp, "pg-1")
	if p := findAssessment(as, "pg-1"); p == nil || p.NeedsHeal {
		t.Fatalf("primary must never need heal; got %+v", p)
	}
}

// A replica on an older timeline than the primary is on a dead lineage — it cannot
// catch up by streaming and must be re-cloned.
func TestCnpgBuildAssessments_BehindTimelineNeedsHeal(t *testing.T) {
	a := cnpgAssess(t, controlData{Pod: "pg-2", Timeline: "8", CheckpointLocation: "40/00000000", Source: "exec"}, nil, nil)
	if !a.NeedsHeal {
		t.Fatalf("an older-timeline replica needs heal (re-clone); notes=%v", a.Notes)
	}
}

// Same timeline, behind by LSN but actively streaming = normal lag; leave it alone.
func TestCnpgBuildAssessments_SameTimelineStreamingNoHeal(t *testing.T) {
	a := cnpgAssess(t, controlData{Pod: "pg-2", Timeline: "9", CheckpointLocation: "40/00000000", Source: "exec"},
		[]string{"pg-2"}, nil)
	if a.NeedsHeal {
		t.Fatalf("a streaming same-timeline replica is normal lag, not a heal; notes=%v", a.Notes)
	}
}

// Same timeline, behind by LSN, NOT streaming and crash-looping = WAL-stranded; its
// only path home is a re-clone.
func TestCnpgBuildAssessments_SameTimelineStrandedNeedsHeal(t *testing.T) {
	a := cnpgAssess(t, controlData{Pod: "pg-2", Timeline: "9", CheckpointLocation: "40/00000000", Source: "exec"},
		nil, []string{"pg-2"})
	if !a.NeedsHeal {
		t.Fatalf("a stranded (behind, not streaming, crash-looping) replica needs heal; notes=%v", a.Notes)
	}
}
