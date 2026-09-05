package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildHelperPod_IstioExemptAndShape(t *testing.T) {
	uid := int64(26)
	pod := BuildHelperPod(HelperPodOpts{
		Name: "p", Namespace: "ns", Image: "img", Command: []string{"sh", "-c", "sleep 1"},
		ServiceAccount: "sa", PVCName: "pvc-1", MountPath: "/data", ReadOnly: true,
		RunAsUID: &uid, RunAsGID: &uid, FSGroup: &uid,
		NodeName: "node-a", Labels: map[string]string{"role": "probe"},
		ActiveDeadlineSeconds: 150,
	})

	// Istio exemption is the property that must never be forgotten.
	if pod.Labels[istioInjectKey] != "false" || pod.Annotations[istioInjectKey] != "false" {
		t.Fatalf("Istio exemption missing: labels=%v annotations=%v", pod.Labels, pod.Annotations)
	}
	if pod.Labels["role"] != "probe" {
		t.Fatalf("caller label dropped: %v", pod.Labels)
	}
	vm := pod.Spec.Containers[0].VolumeMounts[0]
	if vm.MountPath != "/data" || !vm.ReadOnly {
		t.Fatalf("mount wrong: %+v", vm)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "pvc-1" {
		t.Fatalf("pvc wrong: %+v", pod.Spec.Volumes[0])
	}
	if sc := pod.Spec.SecurityContext; sc == nil || *sc.RunAsUser != 26 || *sc.FSGroup != 26 {
		t.Fatalf("security context wrong: %+v", pod.Spec.SecurityContext)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 150 {
		t.Fatalf("activeDeadline wrong: %v", pod.Spec.ActiveDeadlineSeconds)
	}
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("node pin wrong: %v", pod.Spec.NodeSelector)
	}
	if pod.Spec.SecurityContext == nil && len(pod.Spec.Volumes) != 1 {
		t.Fatal("expected exactly one volume")
	}
}

func TestBuildHelperPod_FSGroupOmittedWhenUnset(t *testing.T) {
	// A site that sets RunAsUser but NOT FSGroup must not get FSGroup — it would
	// trigger a slow recursive chown of a large DB PVC on mount.
	root := int64(0)
	pod := BuildHelperPod(HelperPodOpts{Name: "p", Namespace: "ns", Image: "busybox",
		Command: []string{"true"}, PVCName: "v", MountPath: "/m", RunAsUID: &root})
	sc := pod.Spec.SecurityContext
	if sc == nil || *sc.RunAsUser != 0 {
		t.Fatalf("want RunAsUser=0, got %+v", sc)
	}
	if sc.FSGroup != nil {
		t.Fatalf("FSGroup must stay unset when the caller omits it, got %d", *sc.FSGroup)
	}
	if sc.RunAsGroup != nil {
		t.Fatalf("RunAsGroup must stay unset when the caller omits it, got %d", *sc.RunAsGroup)
	}
}

func TestBuildHelperPod_NoUIDStillHardened(t *testing.T) {
	// Even with no RunAsUID, the pod carries the hardening posture (the policy needs it) —
	// but no RunAsUser/RunAsGroup/FSGroup is invented.
	pod := BuildHelperPod(HelperPodOpts{Name: "p", Namespace: "ns", Image: "busybox",
		Command: []string{"true"}, PVCName: "v", MountPath: "/m"})
	sc := pod.Spec.SecurityContext
	if sc == nil {
		t.Fatal("expected a hardened pod SecurityContext (seccomp + runAsNonRoot)")
	}
	if sc.RunAsUser != nil || sc.RunAsGroup != nil || sc.FSGroup != nil {
		t.Fatalf("no UID/GID/FSGroup should be invented, got %+v", sc)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("want seccomp RuntimeDefault, got %+v", sc.SeccompProfile)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatal("want runAsNonRoot=true by default")
	}
	// Exemption still applied even with no other labels.
	if pod.Labels[istioInjectKey] != "false" {
		t.Fatal("exemption must apply even with no caller labels")
	}
}

func TestApplyHelperHardening_StrongestPostureByDefault(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	ApplyHelperHardening(pod, HelperHardening{})

	csc := pod.Spec.Containers[0].SecurityContext
	if csc == nil || csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Fatalf("want allowPrivilegeEscalation=false, got %+v", csc)
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("want capabilities drop [ALL], got %+v", csc.Capabilities)
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Fatalf("want readOnlyRootFilesystem=true, got %+v", csc.ReadOnlyRootFilesystem)
	}
	if pod.Labels[exceptNonRootLabel] != "" || pod.Labels[exceptROFSLabel] != "" {
		t.Fatalf("no exception labels for the strongest posture, got %v", pod.Labels)
	}
}

func TestApplyHelperHardening_ExceptionsAreSelfDocumented(t *testing.T) {
	// A root helper that also writes to rootfs asserts neither dimension, and stamps BOTH
	// exception labels so the policy excludes it visibly.
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	ApplyHelperHardening(pod, HelperHardening{RootRequired: true, WritableRootFS: true})

	if sc := pod.Spec.SecurityContext; sc.RunAsNonRoot != nil {
		t.Fatalf("root-required helper must not assert runAsNonRoot, got %+v", sc.RunAsNonRoot)
	}
	if roc := pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem; roc != nil {
		t.Fatalf("writable-rootfs helper must not assert readOnlyRootFilesystem, got %+v", roc)
	}
	if pod.Labels[exceptNonRootLabel] != "true" || pod.Labels[exceptROFSLabel] != "true" {
		t.Fatalf("both exception labels must be stamped, got %v", pod.Labels)
	}
	// Caps + privesc are hardened regardless of the exceptions.
	csc := pod.Spec.Containers[0].SecurityContext
	if *csc.AllowPrivilegeEscalation || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("caps/privesc must always be hardened, got %+v", csc)
	}
}

func TestApplyHelperHardening_PreservesExistingUID(t *testing.T) {
	// A hand-built pod that already set RunAsUser/FSGroup keeps them; hardening only adds
	// seccomp + runAsNonRoot alongside.
	uid := int64(26)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid},
		Containers:      []corev1.Container{{Name: "c"}},
	}}
	ApplyHelperHardening(pod, HelperHardening{})
	sc := pod.Spec.SecurityContext
	if sc.RunAsUser == nil || *sc.RunAsUser != 26 || sc.FSGroup == nil || *sc.FSGroup != 26 {
		t.Fatalf("existing UID/FSGroup must be preserved, got %+v", sc)
	}
	if sc.SeccompProfile == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("hardening must add seccomp + runAsNonRoot, got %+v", sc)
	}
}

func TestPodUnschedulable(t *testing.T) {
	unsched := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"},
	}}}
	if !PodUnschedulable(unsched) {
		t.Fatal("PodScheduled=False must be unschedulable")
	}
	// Scheduled but waiting on a volume detach is NOT unschedulable (freeing the PVC helps).
	scheduled := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
	}}}
	if PodUnschedulable(scheduled) {
		t.Fatal("a scheduled pod is not unschedulable")
	}
	if PodUnschedulable(nil) {
		t.Fatal("nil pod is not unschedulable")
	}
}

func TestDescribePodScheduling(t *testing.T) {
	unschedulable := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: "0/10 nodes are available: 10 Too many pods.",
		}},
	}}
	if got := DescribePodScheduling(unschedulable); !strings.Contains(got, "Too many pods") || !strings.Contains(got, "unschedulable") {
		t.Fatalf("maxPods case not surfaced: %q", got)
	}

	imgPull := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "back-off pulling image",
			}},
		}},
	}}
	if got := DescribePodScheduling(imgPull); !strings.Contains(got, "ImagePullBackOff") {
		t.Fatalf("image pull not surfaced: %q", got)
	}

	creating := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
	}}
	if got := DescribePodScheduling(creating); !strings.Contains(got, "pending") {
		t.Fatalf("benign ContainerCreating should fall through to pending, got %q", got)
	}

	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if got := DescribePodScheduling(running); got != "" {
		t.Fatalf("a running pod has no scheduling problem, got %q", got)
	}
	if got := DescribePodScheduling(nil); got != "" {
		t.Fatalf("nil pod => empty, got %q", got)
	}
}
