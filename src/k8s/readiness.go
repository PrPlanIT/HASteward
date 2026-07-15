package k8s

import corev1 "k8s.io/api/core/v1"

// Kubernetes reports pod.Status.ContainerStatuses sorted ALPHABETICALLY by name,
// NOT in pod-spec order. So indexing [0] is a bug whenever a pod has more than one
// container: a Galera pod's spec order [mariadb, agent] surfaces as status order
// [agent, mariadb], making ContainerStatuses[0] the agent sidecar. Always resolve
// the container you care about BY NAME with these helpers.

// ContainerStatusByName returns the named container's status, or (nil, false) if
// the pod has no status for that container yet.
func ContainerStatusByName(pod corev1.Pod, name string) (*corev1.ContainerStatus, bool) {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i], true
		}
	}
	return nil, false
}

// ContainerReadyByName reports whether the named container is present and Ready.
func ContainerReadyByName(pod corev1.Pod, name string) bool {
	cs, ok := ContainerStatusByName(pod, name)
	return ok && cs.Ready
}

// PodReady reports whether the pod is Running and its named primary container is
// Ready — the correct "this database node is serving" check.
func PodReady(pod corev1.Pod, container string) bool {
	return pod.Status.Phase == corev1.PodRunning && ContainerReadyByName(pod, container)
}
