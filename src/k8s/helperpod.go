package k8s

import (
	"fmt"
	"strings"

	"github.com/PrPlanIT/HASteward/src/common"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file is the ONE place hasteward builds a short-lived helper pod that mounts a
// single PVC (probe / deep-recover / wsrep_recover / heal / clear). Centralizing it
// guarantees two properties that were previously per-site and easy to forget:
//
//   - Istio sidecar injection is disabled. A sidecar keeps the pod Running forever,
//     so any "wait for Succeeded" hangs to a timeout — which in a meshed namespace
//     silently breaks every authority read (probe, wsrep_recover, deep-recover). The
//     helper only touches a local PVC and needs no mesh, so exclusion is always correct.
//   - Scheduling failures are diagnosable (DescribePodScheduling), so a pod stuck
//     Pending because a node is at its pod cap / cordoned / NotReady is reported as
//     exactly that, instead of an indistinguishable "timed out".

// istioInjectKey is the label/annotation Istio honours to skip sidecar injection.
const istioInjectKey = "sidecar.istio.io/inject"

// Per-workload exception labels the cluster's enforce-pod-hardening ClusterPolicy excludes on.
// A helper stamps the matching label for a dimension it legitimately cannot satisfy, so the
// exception is visible in the live pod instead of the pod silently failing admission.
const (
	exceptNonRootLabel = "policy.prplanit.com/except-non-root"
	exceptROFSLabel    = "policy.prplanit.com/except-ro-rootfs"
)

// DisableIstioInjection stamps the sidecar-exclusion label + annotation onto a pod.
// Safe to call on any hand-built pod; BuildHelperPod applies it automatically.
func DisableIstioInjection(pod *corev1.Pod) {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Labels[istioInjectKey] = "false"
	pod.Annotations[istioInjectKey] = "false"
}

// HelperPodOpts describes a one-shot helper pod mounting exactly one PVC.
type HelperPodOpts struct {
	Name           string
	Namespace      string
	Image          string
	Command        []string
	ServiceAccount string
	PVCName        string
	MountPath      string
	ReadOnly       bool // mount the PVC read-only (inspection must never write)
	// Security context — each field is applied ONLY when non-nil, so a site can set
	// RunAsUser without FSGroup. This matters: FSGroup triggers a recursive chown of
	// the mounted volume, which on a large database PVC is slow and ownership-changing,
	// so sites that deliberately omit it must keep omitting it. All nil => no context.
	RunAsUID *int64
	RunAsGID *int64
	FSGroup  *int64
	NodeName string            // optional: pin to kubernetes.io/hostname=<node>
	Labels   map[string]string // extra labels; the Istio exemption is always added
	// ActiveDeadlineSeconds bounds the pod's own runtime (0 => unset).
	ActiveDeadlineSeconds int64
	// Hardening classifies this helper against the cluster's enforce-pod-hardening policy;
	// BuildHelperPod applies it via ApplyHelperHardening. The zero value is the strongest
	// posture (non-root, read-only rootfs) — set the flags only for a helper that genuinely
	// runs as root or writes outside its mounts. See HelperHardening.
	Hardening HelperHardening
}

// HelperHardening classifies which of the cluster's enforce-pod-hardening dimensions a helper
// can satisfy by construction versus which it legitimately cannot. Every helper is hardened on
// the three dimensions a PVC-mounting pod never needs — ALL capabilities dropped, privilege
// escalation forbidden, seccomp=RuntimeDefault — unconditionally. The two flags cover the
// dimensions a database-recovery helper genuinely cannot meet; each maps to the self-documenting
// except-<dimension> label the policy excludes on, so the exception is visible in the live pod
// instead of the pod silently failing admission.
type HelperHardening struct {
	RootRequired   bool // runs as root (e.g. restic reading every owner's files) → except-non-root
	WritableRootFS bool // runs a DB engine writing outside its mounts (postgres --single, mariadbd, pg_basebackup) → except-ro-rootfs
}

// ApplyHelperHardening stamps the SEC-compliant securityContext + exception labels onto a helper
// pod so it clears the cluster's enforce-pod-hardening admission policy. It is the ONE source of
// truth for hasteward's helper-pod security posture: BuildHelperPod applies it automatically, and
// the hand-built helper pods (escrow, heal, clear, wal-prune, deadlock-recover, galera reconfigure)
// call it directly, so no call site can drift out of policy. Container-level (required on EVERY
// container by the policy): drop ALL capabilities, allowPrivilegeEscalation=false always;
// readOnlyRootFilesystem=true unless WritableRootFS. Pod-level (the policy accepts these
// pod-level): seccompProfile=RuntimeDefault always; runAsNonRoot=true unless RootRequired. Any
// RunAsUser/RunAsGroup/FSGroup a caller already set is preserved.
func ApplyHelperHardening(pod *corev1.Pod, h HelperHardening) {
	if pod == nil {
		return
	}
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	pod.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	if !h.RootRequired {
		pod.Spec.SecurityContext.RunAsNonRoot = common.Ptr(true)
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.AllowPrivilegeEscalation = common.Ptr(false)
		c.SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
		if !h.WritableRootFS {
			c.SecurityContext.ReadOnlyRootFilesystem = common.Ptr(true)
		}
	}
	if h.RootRequired || h.WritableRootFS {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		if h.RootRequired {
			pod.Labels[exceptNonRootLabel] = "true"
		}
		if h.WritableRootFS {
			pod.Labels[exceptROFSLabel] = "true"
		}
	}
}

// BuildHelperPod builds the one-shot PVC-mounting helper pod, with Istio sidecar
// injection disabled. The internal volume name is fixed ("pvc") — it is never
// referenced outside the pod.
func BuildHelperPod(o HelperPodOpts) *corev1.Pod {
	labels := map[string]string{}
	for k, v := range o.Labels {
		labels[k] = v
	}

	spec := corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyNever,
		ServiceAccountName: o.ServiceAccount,
		Containers: []corev1.Container{{
			Name:    "helper",
			Image:   o.Image,
			Command: o.Command,
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "pvc",
				MountPath: o.MountPath,
				ReadOnly:  o.ReadOnly,
			}},
		}},
		Volumes: []corev1.Volume{{
			Name: "pvc",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: o.PVCName},
			},
		}},
	}

	if o.RunAsUID != nil || o.RunAsGID != nil || o.FSGroup != nil {
		spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:  o.RunAsUID,
			RunAsGroup: o.RunAsGID,
			FSGroup:    o.FSGroup,
		}
	}
	if o.ActiveDeadlineSeconds > 0 {
		spec.ActiveDeadlineSeconds = &o.ActiveDeadlineSeconds
	}
	if o.NodeName != "" {
		spec.NodeSelector = map[string]string{"kubernetes.io/hostname": o.NodeName}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace, Labels: labels},
		Spec:       spec,
	}
	DisableIstioInjection(pod)
	ApplyHelperHardening(pod, o.Hardening)
	return pod
}

// PodUnschedulable reports whether the scheduler has positively refused to place the
// pod (PodScheduled=False) — the node-at-pod-cap / cordoned / NotReady / tainted
// case. Distinct from a pod that scheduled but is still pulling an image or waiting
// for its volume to detach: for an Unschedulable helper, freeing a PVC does nothing,
// so callers should stop retrying and fail fast with DescribePodScheduling.
func PodUnschedulable(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return true
		}
	}
	return false
}

// DescribePodScheduling returns a human-readable reason a helper pod is not making
// progress, derived purely from its status — or "" when nothing looks wrong (it may
// simply be starting). Wait loops call this on timeout so a scheduling failure (node
// at its pod cap, cordoned, NotReady, taints) is reported as itself instead of an
// opaque "timed out". Pure and status-only (no API calls), so it is unit-testable.
func DescribePodScheduling(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	// The scheduler could not place the pod — the maxPods / cordon / NotReady / taint case.
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			reason := c.Reason
			if reason == "" {
				reason = "Unschedulable"
			}
			if msg := strings.TrimSpace(c.Message); msg != "" {
				return fmt.Sprintf("unschedulable (%s): %s", reason, msg)
			}
			return fmt.Sprintf("unschedulable (%s)", reason)
		}
	}
	// Container is wedged waiting for a non-benign reason (image pull, volume attach
	// surfaced as a real reason, config error, etc.).
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "", "ContainerCreating", "PodInitializing":
			// benign / in-progress
		default:
			if m := strings.TrimSpace(cs.State.Waiting.Message); m != "" {
				return fmt.Sprintf("container waiting (%s): %s", cs.State.Waiting.Reason, m)
			}
			return fmt.Sprintf("container waiting (%s)", cs.State.Waiting.Reason)
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		return "pending (not yet running — scheduling or volume attach in progress)"
	}
	return ""
}
