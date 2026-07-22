package triage

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWithPostgres(name string, ready bool) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "postgres", Ready: ready}},
		},
	}
}

// TestCnpgIsAnythingAlive pins the gate that must NEVER regress: a serving cluster is
// never treated as belly-up (which would fence it to inspect a replica). Any Ready
// postgres, or ready instances per status, means alive.
func TestCnpgIsAnythingAlive(t *testing.T) {
	cases := []struct {
		name           string
		running        []corev1.Pod
		readyInstances int64
		wantAlive      bool
	}{
		{"status reports ready instances -> alive", nil, 2, true},
		{"a Ready postgres container -> alive", []corev1.Pod{podWithPostgres("pg-1", true)}, 0, true},
		{"one ready among crash-loopers -> alive", []corev1.Pod{podWithPostgres("pg-1", false), podWithPostgres("pg-2", true)}, 0, true},
		{"all crash-looping, no ready instances -> belly-up", []corev1.Pod{podWithPostgres("pg-1", false), podWithPostgres("pg-2", false)}, 0, false},
		{"no pods, no ready instances -> belly-up", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := &cnpgTriageData{runningPods: c.running}
			if got := cnpgIsAnythingAlive(data, c.readyInstances); got != c.wantAlive {
				t.Fatalf("cnpgIsAnythingAlive = %v, want %v", got, c.wantAlive)
			}
		})
	}
}

// TestUnreadBoundInstances: only a present-with-data (Bound PVC) but unread instance
// is a deep-recover target. A read instance, or one whose PVC can't be mounted
// (Pending/MISSING/UNKNOWN), is not — the latter stays unread and keeps blocking.
func TestUnreadBoundInstances(t *testing.T) {
	data := &cnpgTriageData{
		controlData: []controlData{
			{Pod: "pg-1", Timeline: "9"},         // read → not a target
			{Pod: "pg-2", Timeline: "unknown"},   // unread + Bound → target
			{Pod: "pg-3", Timeline: "unknown"},   // unread + Pending → not mountable
			{Pod: "pg-4", Timeline: "unknown"},   // unread + UNKNOWN (transient) → not mountable
			{Pod: "pg-5", Timeline: "unknown"},   // unread + MISSING → nothing to mount
		},
		pvcStates: map[string]string{
			"pg-1": "Bound", "pg-2": "Bound", "pg-3": "Pending", "pg-4": "UNKNOWN", "pg-5": "MISSING",
		},
	}
	got := unreadBoundInstances(data)
	if len(got) != 1 || got[0] != "pg-2" {
		t.Fatalf("want only [pg-2] (unread + Bound), got %v", got)
	}
}
