package galera

import (
	"context"
	"fmt"
	"io"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// healNode heals a single Galera node via suspend/scale/wipe/resume.
func (e *Engine) healNode(ctx context.Context, targetPod string, instanceNum int) error {
	ns := e.cfg.Namespace
	c := k8s.GetClients()

	// Capture SA from target pod before it gets deleted during scale-down
	sa := "default"
	if targetPodObj, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{}); err == nil {
		if targetPodObj.Spec.ServiceAccountName != "" {
			sa = targetPodObj.Spec.ServiceAccountName
		}
	}

	storagePVC := fmt.Sprintf("storage-%s", targetPod)
	galeraPVC := fmt.Sprintf("galera-%s", targetPod)
	storageHelper := fmt.Sprintf("%s-heal-storage-%d-%d", e.cfg.ClusterName, instanceNum, time.Now().Unix())
	galeraHelper := fmt.Sprintf("%s-heal-galera-%d-%d", e.cfg.ClusterName, instanceNum, time.Now().Unix())

	suspended := false
	scaledDown := false
	originalReplicas := int32(e.replicas)

	// Determine scale strategy:
	// If target is the last ordinal (replicas-1), can do partial scale down to that ordinal.
	// Otherwise, must scale to 0 because StatefulSets scale down from highest ordinal.
	scaleTarget := int32(0)
	if instanceNum == int(e.replicas)-1 {
		scaleTarget = int32(instanceNum)
	}
	strategy := "full"
	if scaleTarget > 0 {
		strategy = "partial"
	}

	// Check if galera config PVC exists
	_, galeraErr := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, galeraPVC, metav1.GetOptions{})
	hasGaleraPVC := galeraErr == nil

	output.Section("Healing " + targetPod)
	output.Bullet(0, "Strategy: %s (scale to %d)", strategy, scaleTarget)
	output.Bullet(0, "1. Suspend MariaDB CR (operator stops reconciling)")
	output.Bullet(0, "2. Scale down StatefulSet to %d (release PVCs)", scaleTarget)
	output.Bullet(0, "3. Wipe grastate.dat + galera.cache on storage PVC")
	if hasGaleraPVC {
		output.Bullet(0, "4. Remove bootstrap config from galera PVC")
	} else {
		output.Bullet(0, "4. (no galera PVC)")
	}
	output.Bullet(0, "5. Scale back up, resume CR (node rejoins via SST)")

	// Rescue cleanup function
	rescue := func() {
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, storageHelper, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
		if hasGaleraPVC {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, galeraHelper, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
		}
		if scaledDown {
			e.scaleStatefulSet(ctx, originalReplicas)
		}
		if suspended {
			e.resumeCR(ctx)
		}
		if suspended || scaledDown {
			common.WarnLog("HEAL FAILED for %s. CR resumed and scale restored.", targetPod)
		}
	}

	// STEP 1: Suspend MariaDB CR
	common.InfoLog("STEP 1: Suspending MariaDB CR")
	if err := e.suspendCR(ctx); err != nil {
		return fmt.Errorf("failed to suspend CR: %w", err)
	}
	suspended = true
	time.Sleep(3 * time.Second)

	// STEP 2: Scale down StatefulSet
	common.InfoLog("STEP 2: Scaling StatefulSet to %d", scaleTarget)
	if err := e.scaleStatefulSet(ctx, scaleTarget); err != nil {
		rescue()
		return fmt.Errorf("failed to scale StatefulSet: %w", err)
	}
	scaledDown = true

	// Wait for target pod to terminate
	deleteTimeout := e.cfg.DeleteTimeout
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}

	for i := 0; i < deleteTimeout/5; i++ {
		_, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if err != nil {
			common.InfoLog("Pod %s terminated, PVCs released", targetPod)
			break
		}
		time.Sleep(5 * time.Second)
	}

	// STEP 3: Wipe grastate.dat + galera.cache on storage PVC
	common.InfoLog("STEP 3: Wiping grastate.dat + galera.cache")
	storageScript := `set -e
echo "=== Current grastate.dat ==="
cat /var/lib/mysql/grastate.dat 2>/dev/null || echo "not found"
echo ""
echo "=== Preserving grastate.dat ==="
cp /var/lib/mysql/grastate.dat /var/lib/mysql/grastate.dat.pre-heal 2>/dev/null || echo "nothing to preserve"
echo "=== Wiping grastate.dat ==="
printf '%s\n' \
  '# GALERA saved state' \
  'version: 2.1' \
  'uuid:    00000000-0000-0000-0000-000000000000' \
  'seqno:   -1' \
  'safe_to_bootstrap: 0' \
  > /var/lib/mysql/grastate.dat
echo "New grastate.dat:"
cat /var/lib/mysql/grastate.dat
echo ""
echo "=== Preserving galera.cache ==="
mv /var/lib/mysql/galera.cache /var/lib/mysql/galera.cache.pre-heal 2>/dev/null || echo "no galera.cache to preserve"
echo "=== Done! ==="
`
	if err := e.runHelperPod(ctx, storageHelper, storagePVC, "/var/lib/mysql", storageScript, sa); err != nil {
		rescue()
		return fmt.Errorf("storage helper failed: %w", err)
	}

	// STEP 4: Remove bootstrap config from galera PVC (if exists)
	if hasGaleraPVC {
		common.InfoLog("STEP 4: Removing bootstrap config from galera PVC")
		galeraScript := `set -e
echo "=== Current galera config ==="
ls -la /galera/
echo ""
if [ -f /galera/1-bootstrap.cnf ]; then
  echo "=== Found bootstrap config ==="
  cat /galera/1-bootstrap.cnf
  echo ""
  echo "=== Removing 1-bootstrap.cnf ==="
  rm -f /galera/1-bootstrap.cnf
  echo "Bootstrap config removed!"
else
  echo "No 1-bootstrap.cnf found (OK)"
fi
echo ""
echo "=== Final galera config ==="
ls -la /galera/
echo "=== Done! ==="
`
		if err := e.runHelperPod(ctx, galeraHelper, galeraPVC, "/galera", galeraScript, sa); err != nil {
			rescue()
			return fmt.Errorf("galera config helper failed: %w", err)
		}
	}

	// STEP 5: Scale back up and resume CR
	common.InfoLog("STEP 5: Scaling back up and resuming CR")

	// Clear stale recovery pods
	e.deleteRecoveryPods(ctx)
	time.Sleep(2 * time.Second)

	// Scale back up
	if err := e.scaleStatefulSet(ctx, originalReplicas); err != nil {
		rescue()
		return fmt.Errorf("failed to scale StatefulSet back up: %w", err)
	}
	scaledDown = false

	// Resume CR
	if err := e.resumeCR(ctx); err != nil {
		rescue()
		return fmt.Errorf("failed to resume CR: %w", err)
	}
	suspended = false

	// Wait for pod to come back online
	common.InfoLog("Waiting for %s to come back online", targetPod)
	healTimeout := e.cfg.HealTimeout
	if healTimeout <= 0 {
		healTimeout = 600
	}

	ready := false
	for i := 0; i < healTimeout/10; i++ {
		time.Sleep(10 * time.Second)
		pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == "Running" &&
			len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
			ready = true
			break
		}
	}

	if ready {
		output.Success("Node %s has been healed!", targetPod)
	} else {
		common.WarnLog("%s did not become ready within timeout. SST may still be in progress.", targetPod)
	}

	return nil
}

// suspendCR patches the MariaDB CR to set spec.suspend=true.
func (e *Engine) suspendCR(ctx context.Context) error {
	c := k8s.GetClients()
	patch := `{"spec":{"suspend":true}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(e.cfg.Namespace).Patch(
		ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// resumeCR patches the MariaDB CR to set spec.suspend=false.
func (e *Engine) resumeCR(ctx context.Context) error {
	c := k8s.GetClients()
	patch := `{"spec":{"suspend":false}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(e.cfg.Namespace).Patch(
		ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// scaleStatefulSet scales the StatefulSet to the desired replica count.
func (e *Engine) scaleStatefulSet(ctx context.Context, replicas int32) error {
	c := k8s.GetClients()
	scale, err := c.Clientset.AppsV1().StatefulSets(e.cfg.Namespace).GetScale(
		ctx, e.cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = c.Clientset.AppsV1().StatefulSets(e.cfg.Namespace).UpdateScale(
		ctx, e.cfg.ClusterName, scale, metav1.UpdateOptions{})
	return err
}

// runHelperPod creates a busybox pod that mounts a PVC and runs a script,
// waits for completion, fetches logs, and cleans up.
func (e *Engine) runHelperPod(ctx context.Context, name, pvcName, mountPath, script, sa string) error {
	ns := e.cfg.Namespace
	c := k8s.GetClients()

	rootUser := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "heal-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: sa,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &rootUser,
			},
			Containers: []corev1.Container{{
				Name:    "healer",
				Image:   "docker.io/library/busybox:latest",
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: mountPath},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			}},
		},
	}

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create helper pod %s: %w", name, err)
	}

	// Wait for completion
	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		p, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		phase := string(p.Status.Phase)
		if phase == "Succeeded" {
			e.logHelperPodOutput(ctx, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			time.Sleep(2 * time.Second)
			return nil
		}
		if phase == "Failed" {
			e.logHelperPodOutput(ctx, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
			return fmt.Errorf("helper pod %s failed", name)
		}
	}

	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	return fmt.Errorf("helper pod %s timed out", name)
}

// logHelperPodOutput fetches and displays logs from a helper pod.
func (e *Engine) logHelperPodOutput(ctx context.Context, podName string) {
	c := k8s.GetClients()
	req := c.Clientset.CoreV1().Pods(e.cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		common.DebugLog("Failed to get helper pod logs: %v", err)
		return
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	if len(data) > 0 {
		common.DebugLog("Helper pod output:\n%s", string(data))
	}
}

// deleteRecoveryPods removes stale mariadb-operator recovery pods.
func (e *Engine) deleteRecoveryPods(ctx context.Context) {
	c := k8s.GetClients()
	pods, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + e.cfg.ClusterName + ",k8s.mariadb.com/recovery=true",
	})
	if err != nil {
		return
	}
	for _, p := range pods.Items {
		_ = c.Clientset.CoreV1().Pods(e.cfg.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
	}
}
