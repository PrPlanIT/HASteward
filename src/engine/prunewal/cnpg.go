package prunewal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine/provider"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine/triage"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	Register("cnpg", func(ep provider.EngineProvider) (Pruner, error) {
		p, ok := ep.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("cnpg pruner requires *provider.CNPGProvider, got %T", ep)
		}
		t, err := triage.Get(p)
		if err != nil {
			return nil, fmt.Errorf("cnpg pruner: failed to get triager: %w", err)
		}
		return &cnpgPruner{p: p, triager: t}, nil
	})
}

// cnpgPruner implements Pruner for CloudNativePG PostgreSQL clusters.
type cnpgPruner struct {
	p       *provider.CNPGProvider
	triager triage.Triager
}

func (w *cnpgPruner) Name() string { return "cnpg" }

// PruneWAL clears accumulated WAL from a disk-full CNPG instance.
//
// This is a destructive operation. It is only safe when:
//   - The target instance's replicas are at the same LSN as the primary
//   - The WAL is deadweight held by replication slots that can't advance
//     (typically because replicas were disconnected and are now caught up)
//
// Flow: triage -> safety check -> fence -> mount PVC -> clear pg_wal -> unfence
func (w *cnpgPruner) PruneWAL(ctx context.Context) (*model.PruneWALResult, error) {
	cfg := w.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	if cfg.InstanceNumber == nil {
		return nil, fmt.Errorf("prune wal requires --instance/-i to specify which instance to clear")
	}
	instanceNum := *cfg.InstanceNumber
	targetPod := fmt.Sprintf("%s-%d", cfg.ClusterName, instanceNum)

	result := &model.PruneWALResult{
		Engine:   "cnpg",
		Cluster:  model.ObjectRef{Name: cfg.ClusterName, Namespace: ns},
		Instance: int64(instanceNum),
	}

	// Phase 1: Triage to understand cluster state
	output.Section("Phase 1: Triage")
	triageResult, err := triage.Run(ctx, w.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("triage failed: %w", err)
	}

	// Find the target instance assessment
	var targetAssessment *model.InstanceAssessment
	for i := range triageResult.Assessments {
		if triageResult.Assessments[i].Pod == targetPod {
			targetAssessment = &triageResult.Assessments[i]
			break
		}
	}
	if targetAssessment == nil {
		return nil, fmt.Errorf("instance %s not found in triage", targetPod)
	}

	// Safety checks
	output.Section("Phase 2: Safety Checks")

	// Must be the primary (WAL accumulates on primary, not replicas)
	primary := k8s.GetNestedString(w.p.Cluster(), "status", "currentPrimary")
	if primary != targetPod {
		return nil, fmt.Errorf("ABORT: %s is not the primary (primary is %s). WAL pruning only applies to primaries", targetPod, primary)
	}

	// Must be disk-full or crash-looping
	if targetAssessment.IsReady {
		return nil, fmt.Errorf("ABORT: %s is running and ready. WAL pruning is for disk-full/crash-looping instances", targetPod)
	}

	output.Success("Target %s is primary and not ready — proceeding", targetPod)

	// Check that replicas exist and are reasonably caught up
	// ReadyCount from CNPG status includes the primary, so ready replicas = ReadyCount - (1 if primary is ready, else 0)
	// Since our primary is NOT ready (checked above), ReadyCount == number of healthy replicas
	replicaCount := triageResult.ReadyCount
	if replicaCount == 0 {
		if !cfg.Force {
			return nil, fmt.Errorf("ABORT: no ready replicas found. Cannot verify data safety without at least one healthy replica. Re-run with --force to override")
		}
		common.WarnLog("force=true — proceeding with WAL prune despite no ready replicas. Data safety cannot be verified by a replica.")
	} else {
		output.Success("Found %d ready replica(s)", replicaCount)
	}

	// Resolve PVC name for the target instance
	targetPVC, err := w.resolvePVC(ctx, targetPod)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve PVC for %s: %w", targetPod, err)
	}

	// Discover postgres image and UID/GID from a healthy replica
	imageName, postgresUID, postgresGID, err := w.discoverPostgresInfo(ctx, triageResult)
	if err != nil {
		return nil, fmt.Errorf("failed to discover postgres info: %w", err)
	}

	// Phase 3: Fence and clear WAL
	output.Section("Phase 3: Fence and Clear WAL")
	walPodName := fmt.Sprintf("%s-prune-wal-%d-%d", cfg.ClusterName, instanceNum, time.Now().Unix())

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
			common.WarnLog("To remove fence: kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-", cfg.ClusterName, ns)
		}
	}

	// Step 1: Fence
	output.Bullet(0, "1. Fence instance %s", targetPod)
	if err := w.fenceInstance(ctx, targetPod); err != nil {
		return nil, fmt.Errorf("failed to fence %s: %w", targetPod, err)
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
		return nil, fmt.Errorf("failed to create WAL prune pod: %w", err)
	}
	walPodCreated = true
	time.Sleep(2 * time.Second)

	// Step 3: Aggressively delete target pod until WAL prune pod acquires PVC
	output.Bullet(0, "3. Acquiring PVC from fenced pod")
	deleteTimeout := cfg.DeleteTimeout
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
			w.logHealPodOutput(ctx, walPodName)
			cleanup()
			return nil, fmt.Errorf("WAL prune pod failed before acquiring PVC")
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
		return nil, fmt.Errorf("timeout: WAL prune pod never acquired PVC after %ds", deleteTimeout)
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
			w.logHealPodOutput(ctx, walPodName)
			cleanup()
			return nil, fmt.Errorf("WAL prune pod FAILED")
		}
	}

	if !succeeded {
		w.logHealPodOutput(ctx, walPodName)
		cleanup()
		return nil, fmt.Errorf("WAL prune pod timed out")
	}

	// Display output
	w.logHealPodOutput(ctx, walPodName)

	// Cleanup WAL prune pod
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, walPodName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	walPodCreated = false
	time.Sleep(3 * time.Second)

	// Step 5: Unfence
	output.Bullet(0, "5. Removing fence for %s", targetPod)
	if err := w.unfenceInstance(ctx, targetPod); err != nil {
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
			return result, nil
		}
	}

	common.WarnLog("%s did not become ready within timeout. CNPG may still be reconciling.", targetPod)
	return result, nil
}

// fenceInstance adds a pod to the CNPG fenced instances annotation.
func (w *cnpgPruner) fenceInstance(ctx context.Context, pod string) error {
	c := k8s.GetClients()
	cfg := w.p.Config()

	// Get current fence list
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	annotations := k8s.GetNestedMap(obj, "metadata", "annotations")
	current := provider.ParseFencedInstances(annotations)

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
	_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// unfenceInstance removes a pod from the CNPG fenced instances annotation.
func (w *cnpgPruner) unfenceInstance(ctx context.Context, pod string) error {
	c := k8s.GetClients()
	cfg := w.p.Config()

	// Get current fence list
	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	annotations := k8s.GetNestedMap(obj, "metadata", "annotations")
	current := provider.ParseFencedInstances(annotations)

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
		_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Patch(
			ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	} else {
		fencedJSON, _ := json.Marshal(remaining)
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"cnpg.io/fencedInstances":%q}}}`, string(fencedJSON))
		_, err = c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Patch(
			ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	}
	return err
}

// logHealPodOutput fetches and displays logs from a heal pod.
func (w *cnpgPruner) logHealPodOutput(ctx context.Context, podName string) {
	c := k8s.GetClients()
	cfg := w.p.Config()
	req := c.Clientset.CoreV1().Pods(cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
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

// resolvePVC finds the PVC name for a given CNPG pod.
func (w *cnpgPruner) resolvePVC(ctx context.Context, podName string) (string, error) {
	c := k8s.GetClients()
	cfg := w.p.Config()
	pod, err := c.Clientset.CoreV1().Pods(cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
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
func (w *cnpgPruner) discoverPostgresInfo(ctx context.Context, triageResult *model.TriageResult) (image, uid, gid string, err error) {
	c := k8s.GetClients()
	cfg := w.p.Config()
	ns := cfg.Namespace
	primary := k8s.GetNestedString(w.p.Cluster(), "status", "currentPrimary")

	// Find a non-primary pod that is Running and Ready
	for _, a := range triageResult.Assessments {
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
	image = k8s.GetNestedString(w.p.Cluster(), "status", "image")
	if image == "" {
		return "", "", "", fmt.Errorf("could not determine postgres image from cluster")
	}
	return image, "26", "26", nil // default postgres UID/GID
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}

// parseInt64 parses a string to int64, returning 0 on failure.
func parseInt64(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}
