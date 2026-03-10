package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// healInstance heals a single CNPG replica via fence/clear/basebackup/unfence.
func (e *Engine) healInstance(ctx context.Context, targetPod, targetPVC string, hcfg *healConfig) error {
	ns := e.cfg.Namespace
	c := k8s.GetClients()

	// Derive names
	parts := strings.Split(targetPod, "-")
	instanceSuffix := parts[len(parts)-1]
	healPodName := fmt.Sprintf("%s-heal-%s-%d", e.cfg.ClusterName, instanceSuffix, time.Now().Unix())
	caSecret := e.cfg.ClusterName + "-ca"
	replSecret := e.cfg.ClusterName + "-replication"

	fenceApplied := false
	healPodCreated := false

	output.Section("Healing " + targetPod)
	output.Bullet(0, "1. Fence instance (CNPG stops managing it)")
	output.Bullet(0, "2. Create heal pod, then aggressively delete fenced pod")
	output.Bullet(0, "3. Clear pgdata on PVC %s (PVC preserved)", targetPVC)
	output.Bullet(0, "4. Run pg_basebackup from primary (%s)", hcfg.primaryIP)
	output.Bullet(0, "5. Remove fence (CNPG takes over the replica)")

	// Cleanup function for rescue on error
	cleanup := func() {
		if healPodCreated {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, healPodName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			common.InfoLog("Heal pod %s deleted", healPodName)
		}
		if fenceApplied {
			common.WarnLog("HEAL FAILED - fence left in place for safety. Instance %s is still fenced.", targetPod)
			common.WarnLog("To remove fence: kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-", e.cfg.ClusterName, ns)
		}
	}

	// STEP 1: Fence the instance
	common.InfoLog("STEP 1: Fencing %s", targetPod)
	if err := e.fenceInstance(ctx, targetPod); err != nil {
		return fmt.Errorf("failed to fence %s: %w", targetPod, err)
	}
	fenceApplied = true
	time.Sleep(3 * time.Second)

	// STEP 2: Create heal pod
	common.InfoLog("STEP 2: Creating heal pod %s", healPodName)
	uid, _ := strconv.ParseInt(hcfg.postgresUID, 10, 64)
	gid, _ := strconv.ParseInt(hcfg.postgresGID, 10, 64)

	healScript := fmt.Sprintf(`set -e
echo "=== Step 1: Clearing pgdata ==="
if [ -f /var/lib/postgresql/data/pgdata/PG_VERSION ]; then
  echo "WARNING: Found existing PG_VERSION file. Proceeding with clear..."
fi
rm -rf /var/lib/postgresql/data/pgdata/*
rm -rf /var/lib/postgresql/data/pgdata/.[!.]*
rm -rf /var/lib/postgresql/data/lost+found 2>/dev/null || true
echo "pgdata cleared."

echo "=== Step 2: Setting up TLS certificates ==="
mkdir -p /tmp/certs
cp /certs/ca/ca.crt /tmp/certs/
cp /certs/replication/tls.crt /tmp/certs/
cp /certs/replication/tls.key /tmp/certs/
chmod 600 /tmp/certs/tls.key
echo "TLS certs ready."

echo "=== Step 3: Running pg_basebackup ==="
pg_basebackup -h %s -p 5432 -U streaming_replica \
  -D /var/lib/postgresql/data/pgdata \
  -Fp -Xs -P -R \
  --checkpoint=fast \
  -d "sslmode=verify-ca sslcert=/tmp/certs/tls.crt sslkey=/tmp/certs/tls.key sslrootcert=/tmp/certs/ca.crt"

echo "=== pg_basebackup complete! ==="`, hcfg.primaryIP)

	healPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      healPodName,
			Namespace: ns,
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
				Name:    "healer",
				Image:   hcfg.imageName,
				Command: []string{"sh", "-c", healScript},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
					{Name: "ca-certs", MountPath: "/certs/ca", ReadOnly: true},
					{Name: "replication-certs", MountPath: "/certs/replication", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "pgdata",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: targetPVC,
						},
					},
				},
				{
					Name: "ca-certs",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: caSecret,
							Items: []corev1.KeyToPath{
								{Key: "ca.crt", Path: "ca.crt"},
							},
						},
					},
				},
				{
					Name: "replication-certs",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: replSecret,
							Items: []corev1.KeyToPath{
								{Key: "tls.crt", Path: "tls.crt"},
								{Key: "tls.key", Path: "tls.key"},
							},
						},
					},
				},
			},
		},
	}

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, healPod, metav1.CreateOptions{})
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to create heal pod: %w", err)
	}
	healPodCreated = true
	time.Sleep(2 * time.Second)

	// STEP 3: Aggressively delete target pod until heal pod acquires PVC
	common.InfoLog("STEP 3: Aggressively deleting %s until heal pod acquires PVC", targetPod)
	deleteTimeout := e.cfg.DeleteTimeout
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}

	deleteCount := 0
	acquired := false
	for elapsed := 0; elapsed < deleteTimeout; elapsed++ {
		// Check heal pod status
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, healPodName, metav1.GetOptions{})
		phase := "Pending"
		if hpErr == nil {
			phase = string(hp.Status.Phase)
		}

		if phase == "Running" || phase == "Succeeded" {
			common.InfoLog("Heal pod acquired PVC after %d deletes", deleteCount)
			acquired = true
			break
		}
		if phase == "Failed" {
			e.logHealPodOutput(ctx, healPodName)
			cleanup()
			return fmt.Errorf("heal pod failed before acquiring PVC")
		}

		// Delete target pod
		delErr := c.Clientset.CoreV1().Pods(ns).Delete(ctx, targetPod, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
		if delErr == nil {
			deleteCount++
		}
		if deleteCount > 0 && deleteCount%10 == 0 {
			common.DebugLog("Deleted %d times, heal pod status: %s", deleteCount, phase)
		}

		time.Sleep(1 * time.Second)
	}

	if !acquired {
		e.logHealPodOutput(ctx, healPodName)
		cleanup()
		return fmt.Errorf("timeout: heal pod never acquired PVC after %ds", deleteTimeout)
	}

	// STEP 4: Wait for heal pod to complete pg_basebackup
	common.InfoLog("STEP 4: Waiting for heal pod to complete pg_basebackup")
	healTimeout := e.cfg.HealTimeout
	if healTimeout <= 0 {
		healTimeout = 600
	}

	succeeded := false
	for i := 0; i < healTimeout/10; i++ {
		time.Sleep(10 * time.Second)
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, healPodName, metav1.GetOptions{})
		if hpErr != nil {
			continue
		}
		phase := string(hp.Status.Phase)
		if phase == "Succeeded" {
			common.InfoLog("Heal pod completed successfully")
			succeeded = true
			break
		}
		if phase == "Failed" {
			e.logHealPodOutput(ctx, healPodName)
			cleanup()
			return fmt.Errorf("heal pod FAILED for %s", targetPod)
		}
		if i > 0 && i%6 == 0 {
			common.InfoLog("Heal pod still running... (%ds elapsed)", (i+1)*10)
		}
	}

	if !succeeded {
		e.logHealPodOutput(ctx, healPodName)
		cleanup()
		return fmt.Errorf("heal pod timed out after %ds for %s", healTimeout, targetPod)
	}

	// Fetch and display heal pod logs
	e.logHealPodOutput(ctx, healPodName)

	// Cleanup heal pod
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, healPodName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	healPodCreated = false
	time.Sleep(5 * time.Second)

	// STEP 5: Remove fence (only our target, preserve others)
	common.InfoLog("STEP 5: Removing fence for %s", targetPod)
	if err := e.unfenceInstance(ctx, targetPod); err != nil {
		common.WarnLog("Failed to unfence %s: %v", targetPod, err)
	}
	fenceApplied = false

	// Delete old pod to clear CrashLoopBackOff history
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, targetPod, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	time.Sleep(5 * time.Second)

	// Wait for pod to come back online
	common.InfoLog("Waiting for %s to come back online", targetPod)
	for i := 0; i < 30; i++ {
		time.Sleep(10 * time.Second)
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if podErr == nil && pod.Status.Phase == "Running" &&
			len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
			output.Success("Replica %s has been healed!", targetPod)
			return nil
		}
	}

	common.WarnLog("%s did not become ready within timeout. CNPG may still be reconciling.", targetPod)
	return nil
}

// fenceInstance appends a pod to the fenced instances list.
func (e *Engine) fenceInstance(ctx context.Context, pod string) error {
	c := k8s.GetClients()

	// Get current fence list
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Get(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	annotations := k8s.GetNestedMap(obj, "metadata", "annotations")
	current := parseFencedInstances(annotations)

	// Check if already fenced
	for _, f := range current {
		if f == pod {
			common.InfoLog("Instance %s already fenced", pod)
			return nil
		}
	}

	// Add to list
	newList := append(current, pod)
	fencedJSON, _ := json.Marshal(newList)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"cnpg.io/fencedInstances":%q}}}`, string(fencedJSON))
	_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Patch(
		ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// unfenceInstance removes a pod from the fenced instances list.
func (e *Engine) unfenceInstance(ctx context.Context, pod string) error {
	c := k8s.GetClients()

	// Get current fence list
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Get(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	annotations := k8s.GetNestedMap(obj, "metadata", "annotations")
	current := parseFencedInstances(annotations)

	// Remove target
	var remaining []string
	for _, f := range current {
		if f != pod {
			remaining = append(remaining, f)
		}
	}

	if len(remaining) == 0 {
		// Remove annotation entirely
		patch := `{"metadata":{"annotations":{"cnpg.io/fencedInstances":null}}}`
		_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Patch(
			ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	} else {
		fencedJSON, _ := json.Marshal(remaining)
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"cnpg.io/fencedInstances":%q}}}`, string(fencedJSON))
		_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(e.cfg.Namespace).Patch(
			ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	}
	return err
}

// logHealPodOutput fetches and displays logs from a heal pod.
func (e *Engine) logHealPodOutput(ctx context.Context, podName string) {
	c := k8s.GetClients()
	req := c.Clientset.CoreV1().Pods(e.cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		common.DebugLog("Failed to get heal pod logs: %v", err)
		return
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	if len(data) > 0 {
		common.InfoLog("Heal pod output:\n%s", string(data))
	}
}
