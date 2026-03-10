package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (e *Engine) Restore(ctx context.Context) (*model.RestoreResult, error) {
	if e.cfg.BackupMethod == "native" {
		return nil, fmt.Errorf("native S3 restore (PITR via bootstrap.recovery) is not yet implemented. Use --method dump")
	}
	return e.restoreDump(ctx)
}

func (e *Engine) restoreDump(ctx context.Context) (*model.RestoreResult, error) {
	start := time.Now()
	ns := e.cfg.Namespace
	primary := k8s.GetNestedString(e.cluster, "status", "currentPrimary")

	snapshotID := e.cfg.Snapshot
	if snapshotID == "" {
		snapshotID = "latest"
	}

	rc := e.newResticClient()
	// Diverged snapshots use ordinal-prefixed filename
	dumpFile := DumpFilename
	if e.cfg.InstanceNumber != nil {
		dumpFile = strconv.Itoa(*e.cfg.InstanceNumber) + "-" + dumpFile
	}
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, e.cfg.ClusterName, dumpFile)
	filterTags := map[string]string{
		"engine":    "cnpg",
		"cluster":   e.cfg.ClusterName,
		"namespace": ns,
	}

	output.Section("Dump Restore")
	output.Field("Snapshot", snapshotID)
	output.Field("Primary", primary)
	output.Field("Repository", e.cfg.BackupsPath)

	// Verify primary is running and ready
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, primary, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("primary pod %s not found: %w", primary, err)
	}
	if pod.Status.Phase != "Running" || len(pod.Status.ContainerStatuses) == 0 || !pod.Status.ContainerStatuses[0].Ready {
		return nil, fmt.Errorf("primary pod %s is not running and ready", primary)
	}

	// Get replica instance names (non-primary)
	var replicas []string
	if names := k8s.GetNestedSlice(e.cluster, "status", "instanceNames"); names != nil {
		for _, n := range names {
			if s, ok := n.(string); ok && s != primary {
				replicas = append(replicas, s)
			}
		}
	}

	// Fence replicas before restore
	if len(replicas) > 0 {
		common.InfoLog("Fencing replicas: %s", strings.Join(replicas, ", "))
		fencedJSON, _ := json.Marshal(replicas)
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"cnpg.io/fencedInstances":%q}}}`, string(fencedJSON))
		_, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Patch(
			ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to fence replicas: %w", err)
		}
	}

	// Set up pipe: restic dump → pipe → psql stdin
	pr, pw := io.Pipe()

	var resticErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		resticErr = rc.Dump(ctx, snapshotID, stdinFilename, pw, filterTags)
		if resticErr != nil {
			pw.CloseWithError(resticErr)
		}
	}()

	common.InfoLog("Streaming restic dump → psql")
	err = k8s.ExecStream(ctx, primary, ns, "postgres",
		[]string{"psql", "-U", "postgres"},
		pr, output.Writer(), os.Stderr)
	<-done

	if err != nil {
		e.unfenceAll(ctx, ns)
		return nil, fmt.Errorf("restore stream failed: %w", err)
	}
	if resticErr != nil {
		e.unfenceAll(ctx, ns)
		return nil, fmt.Errorf("restic dump failed: %w", resticErr)
	}

	output.Section("Restore Complete")

	// Unfence replicas
	if len(replicas) > 0 {
		e.unfenceAll(ctx, ns)

		// Delete replica pods to force clean re-sync
		for _, replica := range replicas {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, replica, metav1.DeleteOptions{
				GracePeriodSeconds: ptr(int64(0)),
			})
		}
		common.InfoLog("Replicas unfenced and deleted — they will re-sync from primary via streaming replication")
	}

	output.Success("Restore complete")
	return &model.RestoreResult{
		Engine:     e.Name(),
		Cluster:    model.ObjectRef{Namespace: ns, Name: e.cfg.ClusterName},
		SnapshotID: snapshotID,
		Duration:   time.Since(start),
	}, nil
}

func (e *Engine) unfenceAll(ctx context.Context, ns string) {
	c := k8s.GetClients()
	patch := `{"metadata":{"annotations":{"cnpg.io/fencedInstances":"[]"}}}`
	_, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Patch(
		ctx, e.cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		common.WarnLog("Failed to unfence replicas: %v", err)
	}
}
