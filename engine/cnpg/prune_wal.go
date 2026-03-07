package cnpg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/output"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PruneWAL clears accumulated WAL from a disk-full CNPG instance.
//
// This is a destructive operation. It is only safe when:
//   - The target instance's replicas are at the same LSN as the primary
//   - The WAL is deadweight held by replication slots that can't advance
//     (typically because replicas were disconnected and are now caught up)
//
// Flow: triage → safety check → fence → mount PVC → clear pg_wal → unfence
func (e *Engine) PruneWAL(ctx context.Context) error {
	ns := e.cfg.Namespace
	c := k8s.GetClients()

	if e.cfg.InstanceNumber == nil {
		return fmt.Errorf("prune wal requires --instance/-i to specify which instance to clear")
	}
	instanceNum := *e.cfg.InstanceNumber
	targetPod := fmt.Sprintf("%s-%d", e.cfg.ClusterName, instanceNum)

	// Phase 1: Triage to understand cluster state
	output.Section("Phase 1: Triage")
	triageResult, err := e.Triage(ctx)
	if err != nil {
		return fmt.Errorf("triage failed: %w", err)
	}

	// Find the target instance assessment
	var targetAssessment *common.InstanceAssessment
	for i := range triageResult.Assessments {
		if triageResult.Assessments[i].Pod == targetPod {
			targetAssessment = &triageResult.Assessments[i]
			break
		}
	}
	if targetAssessment == nil {
		return fmt.Errorf("instance %s not found in triage", targetPod)
	}

	// Safety checks
	output.Section("Phase 2: Safety Checks")

	// Must be the primary (WAL accumulates on primary, not replicas)
	primary := k8s.GetNestedString(e.cluster, "status", "currentPrimary")
	if primary != targetPod {
		return fmt.Errorf("ABORT: %s is not the primary (primary is %s). WAL pruning only applies to primaries", targetPod, primary)
	}

	// Must be disk-full or crash-looping
	if targetAssessment.IsReady {
		return fmt.Errorf("ABORT: %s is running and ready. WAL pruning is for disk-full/crash-looping instances", targetPod)
	}

	output.Success("Target %s is primary and not ready — proceeding", targetPod)

	// Check that replicas exist and are reasonably caught up
	// ReadyCount from CNPG status includes the primary, so ready replicas = ReadyCount - (1 if primary is ready, else 0)
	// Since our primary is NOT ready (checked above), ReadyCount == number of healthy replicas
	replicaCount := triageResult.ReadyCount
	if replicaCount == 0 {
		return fmt.Errorf("ABORT: no ready replicas found. Cannot verify data safety without at least one healthy replica")
	}
	output.Success("Found %d ready replica(s)", replicaCount)

	// Resolve PVC name for the target instance
	targetPVC, err := e.resolvePVC(ctx, targetPod)
	if err != nil {
		return fmt.Errorf("failed to resolve PVC for %s: %w", targetPod, err)
	}

	// Discover postgres image and UID/GID from a healthy replica
	imageName, postgresUID, postgresGID, err := e.discoverPostgresInfo(ctx, triageResult)
	if err != nil {
		return fmt.Errorf("failed to discover postgres info: %w", err)
	}

	// Phase 3: Fence and clear WAL
	output.Section("Phase 3: Fence and Clear WAL")
	walPodName := fmt.Sprintf("%s-prune-wal-%d-%d", e.cfg.ClusterName, instanceNum, time.Now().Unix())

	walScript := `set -e
PGDATA="/var/lib/postgresql/data/pgdata"
WAL_DIR="$PGDATA/pg_wal"

if [ ! -d "$WAL_DIR" ]; then
  echo "ERROR: pg_wal directory not found"
  exit 1
fi

echo "=== Checking pg_wal size ==="
WAL_SIZE=$(du -sh "$WAL_DIR" 2>/dev/null | cut -f1)
WAL_COUNT=$(find "$WAL_DIR" -maxdepth 1 -type f -name '0*' | wc -l)
echo "pg_wal size: $WAL_SIZE ($WAL_COUNT WAL segments)"
TOTAL_SIZE=$(du -sh "$PGDATA" 2>/dev/null | cut -f1)
echo "Total pgdata size: $TOTAL_SIZE"

echo "=== Identifying checkpoint WAL segment ==="
REDO_WAL=$(pg_controldata "$PGDATA" 2>/dev/null | grep "REDO WAL file" | awk '{print $NF}')
if [ -z "$REDO_WAL" ]; then
  echo "ERROR: could not determine checkpoint REDO WAL file from pg_controldata"
  exit 1
fi
echo "Checkpoint REDO WAL file: $REDO_WAL"

echo "=== Clearing WAL segments older than $REDO_WAL ==="
DELETED=0
KEPT=0
# Match only 24-hex-char WAL segment filenames (e.g. 000000030000000A00000036)
# Excludes .history files (e.g. 00000003.history) which pg_rewind needs for timeline tracking
for f in $(find "$WAL_DIR" -maxdepth 1 -type f -regex '.*/[0-9A-F]\{24\}$' | sort); do
  BASENAME=$(basename "$f")
  if [ "$BASENAME" \< "$REDO_WAL" ]; then
    rm -f "$f"
    DELETED=$((DELETED + 1))
  else
    KEPT=$((KEPT + 1))
  fi
done

HISTORY_COUNT=$(find "$WAL_DIR" -maxdepth 1 -type f -name '*.history' | wc -l)
echo "Preserved $HISTORY_COUNT .history file(s) (required for pg_rewind timeline tracking)"

# Remove stale .partial and .backup files (safe — these are bookkeeping, not data)
find "$WAL_DIR" -maxdepth 1 -type f -name '*.partial' -delete
find "$WAL_DIR" -maxdepth 1 -type f -name '*.backup' -delete

echo "Deleted $DELETED WAL segments, kept $KEPT (>= $REDO_WAL)"
WAL_REMAINING=$(du -sh "$WAL_DIR" 2>/dev/null | cut -f1)
TOTAL_REMAINING=$(du -sh "$PGDATA" 2>/dev/null | cut -f1)
echo "pg_wal after prune: $WAL_REMAINING"
echo "Total pgdata after prune: $TOTAL_REMAINING"
echo "=== WAL prune complete ==="
`

	fenceApplied := false
	walPodCreated := false

	cleanup := func() {
		if walPodCreated {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, walPodName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
		}
		if fenceApplied {
			common.WarnLog("WAL prune interrupted — fence left in place for safety. Instance %s is still fenced.", targetPod)
			common.WarnLog("To remove fence: kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-", e.cfg.ClusterName, ns)
		}
	}

	// Step 1: Fence
	output.Bullet(0, "1. Fence instance %s", targetPod)
	if err := e.fenceInstance(ctx, targetPod); err != nil {
		return fmt.Errorf("failed to fence %s: %w", targetPod, err)
	}
	fenceApplied = true
	time.Sleep(3 * time.Second)

	// Step 2: Create WAL prune pod
	output.Bullet(0, "2. Create WAL prune pod to clear pg_wal")

	uid, gid := parseInt64(postgresUID), parseInt64(postgresGID)

	walPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      walPodName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
				FSGroup:    &gid,
			},
			Containers: []corev1.Container{{
				Name:    "wal-prune",
				Image:   imageName,
				Command: []string{"sh", "-c", walScript},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
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
			},
		},
	}

	_, err = c.Clientset.CoreV1().Pods(ns).Create(ctx, walPod, metav1.CreateOptions{})
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to create WAL prune pod: %w", err)
	}
	walPodCreated = true
	time.Sleep(2 * time.Second)

	// Step 3: Aggressively delete target pod until WAL prune pod acquires PVC
	output.Bullet(0, "3. Acquiring PVC from fenced pod")
	deleteTimeout := e.cfg.DeleteTimeout
	if deleteTimeout <= 0 {
		deleteTimeout = 300
	}

	acquired := false
	deleteCount := 0
	for elapsed := 0; elapsed < deleteTimeout; elapsed++ {
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, walPodName, metav1.GetOptions{})
		phase := "Pending"
		if hpErr == nil {
			phase = string(hp.Status.Phase)
		}
		if phase == "Running" || phase == "Succeeded" {
			common.InfoLog("WAL prune pod acquired PVC after %d deletes", deleteCount)
			acquired = true
			break
		}
		if phase == "Failed" {
			e.logHealPodOutput(ctx, walPodName)
			cleanup()
			return fmt.Errorf("WAL prune pod failed before acquiring PVC")
		}
		delErr := c.Clientset.CoreV1().Pods(ns).Delete(ctx, targetPod, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
		if delErr == nil {
			deleteCount++
		}
		time.Sleep(1 * time.Second)
	}

	if !acquired {
		cleanup()
		return fmt.Errorf("timeout: WAL prune pod never acquired PVC after %ds", deleteTimeout)
	}

	// Step 4: Wait for WAL prune to complete
	output.Bullet(0, "4. Waiting for WAL prune to complete")
	succeeded := false
	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		hp, hpErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, walPodName, metav1.GetOptions{})
		if hpErr != nil {
			continue
		}
		if string(hp.Status.Phase) == "Succeeded" {
			succeeded = true
			break
		}
		if string(hp.Status.Phase) == "Failed" {
			e.logHealPodOutput(ctx, walPodName)
			cleanup()
			return fmt.Errorf("WAL prune pod FAILED")
		}
	}

	if !succeeded {
		e.logHealPodOutput(ctx, walPodName)
		cleanup()
		return fmt.Errorf("WAL prune pod timed out")
	}

	// Display output
	e.logHealPodOutput(ctx, walPodName)

	// Cleanup WAL prune pod
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, walPodName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	walPodCreated = false
	time.Sleep(3 * time.Second)

	// Step 5: Unfence
	output.Bullet(0, "5. Removing fence for %s", targetPod)
	if err := e.unfenceInstance(ctx, targetPod); err != nil {
		common.WarnLog("Failed to unfence %s: %v", targetPod, err)
	}
	fenceApplied = false

	// Delete old pod to clear CrashLoopBackOff
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, targetPod, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	time.Sleep(5 * time.Second)

	// Wait for pod to come back
	output.Bullet(0, "6. Waiting for %s to come back online", targetPod)
	for i := 0; i < 30; i++ {
		time.Sleep(10 * time.Second)
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if podErr == nil && pod.Status.Phase == "Running" &&
			len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
			output.Success("Instance %s is back online!", targetPod)
			return nil
		}
	}

	common.WarnLog("%s did not become ready within timeout. CNPG may still be reconciling.", targetPod)
	return nil
}

// resolvePVC finds the PVC name for a given CNPG pod.
func (e *Engine) resolvePVC(ctx context.Context, podName string) (string, error) {
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		// Pod might be gone (fenced/deleted), try naming convention
		// CNPG PVC name = pod name
		return podName, nil
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "pgdata" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName, nil
		}
	}
	// Fallback: CNPG convention is PVC name = pod name
	return podName, nil
}

// discoverPostgresInfo finds the postgres image, UID, and GID from a healthy instance.
func (e *Engine) discoverPostgresInfo(ctx context.Context, triage *common.TriageResult) (image, uid, gid string, err error) {
	c := k8s.GetClients()
	ns := e.cfg.Namespace
	primary := k8s.GetNestedString(e.cluster, "status", "currentPrimary")

	// Find a non-primary pod that is Running and Ready
	for _, a := range triage.Assessments {
		if a.Pod == primary {
			continue
		}
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, a.Pod, metav1.GetOptions{})
		if podErr != nil {
			continue
		}
		if pod.Status.Phase != "Running" || len(pod.Status.ContainerStatuses) == 0 || !pod.Status.ContainerStatuses[0].Ready {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "postgres" {
				image = container.Image
				break
			}
		}
		if image == "" {
			continue
		}

		// Get UID/GID from running process
		uidResult, uidErr := k8s.ExecCommand(ctx, a.Pod, ns, "postgres", []string{"id", "-u"})
		gidResult, gidErr := k8s.ExecCommand(ctx, a.Pod, ns, "postgres", []string{"id", "-g"})
		if uidErr == nil && gidErr == nil {
			uid = strings.TrimSpace(uidResult.Stdout)
			gid = strings.TrimSpace(gidResult.Stdout)
			if uid != "" && gid != "" {
				return image, uid, gid, nil
			}
		}
	}

	// Fallback to cluster spec image
	image = k8s.GetNestedString(e.cluster, "status", "image")
	if image == "" {
		return "", "", "", fmt.Errorf("could not determine postgres image from cluster")
	}
	return image, "26", "26", nil // default postgres UID/GID
}

func parseInt64(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}
