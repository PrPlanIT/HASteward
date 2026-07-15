package k8s

import (
	"context"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFindCondition(t *testing.T) {
	status := map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True"},
			map[string]interface{}{"type": "GaleraReady", "status": "False"},
		},
	}
	if c := FindCondition(status, "GaleraReady"); c == nil || c["status"] != "False" {
		t.Fatalf("GaleraReady lookup: got %v", c)
	}
	if FindCondition(status, "Ready")["status"] != "True" {
		t.Fatal("Ready lookup failed")
	}
	if FindCondition(status, "Nope") != nil {
		t.Fatal("absent condition type must be nil")
	}
	if FindCondition(map[string]interface{}{}, "Ready") != nil {
		t.Fatal("status with no conditions key must be nil")
	}
}

func TestWaitForPodGone(t *testing.T) {
	defer common.DisableSleepForTest()()

	t.Run("absent pod -> nil immediately", func(t *testing.T) {
		defer fakeClients()()
		if err := WaitForPodGone(context.Background(), "ns", "gone", 3, 1); err != nil {
			t.Fatalf("an absent pod should return nil, got %v", err)
		}
	})

	t.Run("present pod -> error after attempts", func(t *testing.T) {
		defer fakeClients(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "here", Namespace: "ns"}})()
		if err := WaitForPodGone(context.Background(), "ns", "here", 2, 1); err == nil {
			t.Fatal("a still-present pod should return an error after all attempts")
		}
	})
}
