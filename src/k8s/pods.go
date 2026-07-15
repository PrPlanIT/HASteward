package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FindReadyPod returns the name of the first pod matching selector whose named
// container is Running+Ready, or an error if none qualifies. Readiness is judged
// by container NAME, not index (see readiness.go).
func FindReadyPod(ctx context.Context, namespace, selector, container string) (string, error) {
	pods, err := GetClients().Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}
	for _, pod := range pods.Items {
		if PodReady(pod, container) {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no ready pod found for selector %q in namespace %s", selector, namespace)
}

// WaitAllReady polls up to `attempts` times, `intervalSec` seconds apart, until
// `expected` pods matching selector have their named container Running+Ready.
// Returns true if that count is reached. Progress is logged at debug level;
// callers own their success/timeout messaging so engine-specific wording is kept.
func WaitAllReady(ctx context.Context, namespace, selector string, expected, attempts, intervalSec int, container string) bool {
	for i := 0; i < attempts; i++ {
		pods, err := GetClients().Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err == nil {
			ready := 0
			for _, p := range pods.Items {
				if PodReady(p, container) {
					ready++
				}
			}
			if ready == expected {
				return true
			}
			common.DebugLog("Ready: %d/%d", ready, expected)
		}
		time.Sleep(time.Duration(intervalSec) * time.Second)
	}
	return false
}
