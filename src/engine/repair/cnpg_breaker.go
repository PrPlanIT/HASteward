package repair

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clearDatadirOffline clears a disposable instance's pgdata IN PLACE while it is
// down. It is the offline half of healInstance — fence → a clear pod acquires the
// PVC → rm -rf pgdata → unfence — with STEP 4 (pg_basebackup) and the TLS cert
// mounts removed, because the deadlock breaker runs BEFORE any primary exists.
// The PVC is preserved; only its contents are cleared, which frees the disk that
// froze the cluster. Once unfenced and the primary boots, CNPG re-clones the now
// empty replica (or the normal repair heal does).
//
// This is a NARROW EXTRACTION of healInstance, not a reimplementation: it reuses
// fenceInstance/unfenceInstance, the same aggressive-delete-until-the-clear-pod-
// acquires-the-PVC loop, and the same fenced-on-failure safety — on ANY error the
// instance is left fenced so it cannot race the cluster.
//
// CALLER CONTRACT: the breaker MUST gate every call behind
// RecoveryProof.AuthorizesClear(targetPod). clearDatadirOffline does not
// re-derive disposability or authority; it clears exactly the pod/PVC it is given.
func (r *cnpgRepair) clearDatadirOffline(ctx context.Context, targetPod, targetPVC string, hcfg *healConfig) error {
	cfg := r.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	parts := strings.Split(targetPod, "-")
	instanceSuffix := parts[len(parts)-1]
	clearPodName := fmt.Sprintf("%s-unwedge-%s-%d", cfg.ClusterName, instanceSuffix, time.Now().Unix())

	fenceApplied := false
	clearPodCreated := false

	output.Section("Clearing datadir offline: " + targetPod)
	output.Bullet(0, "1. Fence instance (CNPG stops managing it)")
	output.Bullet(0, "2. Create clear pod, then aggressively delete fenced pod")
	output.Bullet(0, "3. Clear pgdata on PVC %s (PVC preserved; no basebackup)", targetPVC)
	output.Bullet(0, "4. Remove fence (CNPG re-clones the empty replica once a primary is up)")

	// Rescue on error: drop the clear pod, and LEAVE THE FENCE so a half-cleared
	// instance cannot be managed by CNPG until an operator looks. Same contract as
	// healInstance.
	cleanup := func() {
		if clearPodCreated {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, clearPodName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
		}
		if fenceApplied {
			common.WarnLog("CLEAR FAILED - fence left in place for safety. Instance %s is still fenced.", targetPod)
			common.WarnLog("To remove fence: kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-", cfg.ClusterName, ns)
		}
	}

	// STEP 1: Fence the instance.
	common.InfoLog("STEP 1: Fencing %s", targetPod)
	if err := r.fenceInstance(ctx, targetPod); err != nil {
		return fmt.Errorf("failed to fence %s: %w", targetPod, err)
	}
	fenceApplied = true
	time.Sleep(3 * time.Second)

	// STEP 2: Create the clear-only pod (pgdata only — no certs, no basebackup).
	common.InfoLog("STEP 2: Creating clear pod %s", clearPodName)
	uid, _ := strconv.ParseInt(hcfg.postgresUID, 10, 64)
	gid, _ := strconv.ParseInt(hcfg.postgresGID, 10, 64)

	clearScript := `set -e
echo "=== Clearing pgdata ==="
if [ -f /var/lib/postgresql/data/pgdata/PG_VERSION ]; then
  echo "WARNING: Found existing PG_VERSION file. Proceeding with clear..."
fi
rm -rf /var/lib/postgresql/data/pgdata/*
rm -rf /var/lib/postgresql/data/pgdata/.[!.]*
rm -rf /var/lib/postgresql/data/lost+found 2>/dev/null || true
echo "pgdata cleared."`

	clearPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clearPodName,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "unwedge-clear"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: hcfg.serviceAccount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
				FSGroup:    &gid,
			},
			Containers: []corev1.Container{{
				Name:    "clearer",
				Image:   hcfg.imageName,
				Command: []string{"sh", "-c", clearScript},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "pgdata",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: targetPVC,
					},
				},
			}},
		},
	}

	if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, clearPod, metav1.CreateOptions{}); err != nil {
		cleanup()
		return fmt.Errorf("failed to create clear pod: %w", err)
	}
	clearPodCreated = true
	time.Sleep(2 * time.Second)

	// STEP 3: Aggressively delete the fenced pod until the clear pod acquires the
	// PVC (RWO: the old pod must release it first). Verbatim from healInstance.
	common.InfoLog("STEP 3: Aggressively deleting %s until clear pod acquires PVC", targetPod)
	deleteTimeout := cfg.DeleteTimeout
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}
	deleteCount := 0
	acquired := false
	for elapsed := 0; elapsed < deleteTimeout; elapsed++ {
		cp, cpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, clearPodName, metav1.GetOptions{})
		phase := "Pending"
		if cpErr == nil {
			phase = string(cp.Status.Phase)
		}
		if phase == "Running" || phase == "Succeeded" {
			common.InfoLog("Clear pod acquired PVC after %d deletes", deleteCount)
			acquired = true
			break
		}
		if phase == "Failed" {
			r.logHealPodOutput(ctx, clearPodName)
			cleanup()
			return fmt.Errorf("clear pod failed before acquiring PVC")
		}
		if delErr := c.Clientset.CoreV1().Pods(ns).Delete(ctx, targetPod, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		}); delErr == nil {
			deleteCount++
		}
		time.Sleep(1 * time.Second)
	}
	if !acquired {
		r.logHealPodOutput(ctx, clearPodName)
		cleanup()
		return fmt.Errorf("timeout: clear pod never acquired PVC after %ds", deleteTimeout)
	}

	// STEP 4: Wait for the clear to complete. rm -rf is fast, but a multi-GB WAL
	// tree can take a while; bound it at 5 min.
	common.InfoLog("STEP 4: Waiting for pgdata clear to complete")
	cleared := false
	for i := 0; i < 60; i++ {
		time.Sleep(5 * time.Second)
		cp, cpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, clearPodName, metav1.GetOptions{})
		if cpErr != nil {
			continue
		}
		if cp.Status.Phase == corev1.PodSucceeded {
			cleared = true
			break
		}
		if cp.Status.Phase == corev1.PodFailed {
			r.logHealPodOutput(ctx, clearPodName)
			cleanup()
			return fmt.Errorf("clear pod FAILED for %s", targetPod)
		}
	}
	if !cleared {
		r.logHealPodOutput(ctx, clearPodName)
		cleanup()
		return fmt.Errorf("clear pod timed out for %s", targetPod)
	}

	r.logHealPodOutput(ctx, clearPodName)

	// Drop the clear pod, then unfence so CNPG re-manages the now-empty replica.
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, clearPodName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	clearPodCreated = false
	time.Sleep(3 * time.Second)

	common.InfoLog("STEP 5: Removing fence for %s", targetPod)
	if err := r.unfenceInstance(ctx, targetPod); err != nil {
		common.WarnLog("Failed to unfence %s: %v", targetPod, err)
	}
	fenceApplied = false

	// Delete the old pod to clear CrashLoopBackOff history; CNPG recreates it empty.
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, targetPod, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})

	output.Success("Cleared pgdata for %s (PVC %s preserved, now empty)", targetPod, targetPVC)
	return nil
}
