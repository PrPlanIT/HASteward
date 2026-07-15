package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/restic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DumpFilenameCNPG is the virtual filename used in restic snapshots for pg_dumpall output.
const DumpFilenameCNPG = "pgdumpall.sql"

func init() {
	Register("cnpg", func(ep provider.EngineProvider) (Restorer, error) {
		p, ok := ep.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("restore/cnpg: expected *provider.CNPGProvider, got %T", ep)
		}
		return &cnpgRestore{p: p}, nil
	})
}

type cnpgRestore struct {
	p *provider.CNPGProvider
}

func (r *cnpgRestore) Name() string { return r.p.Name() }

func (r *cnpgRestore) Restore(ctx context.Context) (*model.RestoreResult, error) {
	cfg := r.p.Config()
	if cfg.BackupMethod == "native" {
		return nil, fmt.Errorf("native S3 restore (PITR via bootstrap.recovery) is not yet implemented. Use --method dump")
	}
	return r.restoreDump(ctx)
}

func (r *cnpgRestore) restoreDump(ctx context.Context) (*model.RestoreResult, error) {
	start := time.Now()
	cfg := r.p.Config()
	ns := cfg.Namespace
	primary := k8s.GetNestedString(r.p.Cluster(), "status", "currentPrimary")

	snapshotID := cfg.Snapshot
	if snapshotID == "" {
		snapshotID = "latest"
	}

	rc := restic.NewClient(cfg.BackupsPath, cfg.ResticPassword)
	// Diverged snapshots use ordinal-prefixed filename
	dumpFile := DumpFilenameCNPG
	if cfg.InstanceNumber != nil {
		dumpFile = strconv.Itoa(*cfg.InstanceNumber) + "-" + dumpFile
	}
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, cfg.ClusterName, dumpFile)
	filterTags := map[string]string{
		"engine":    "cnpg",
		"cluster":   cfg.ClusterName,
		"namespace": ns,
	}

	output.Section("Dump Restore")
	output.Field("Snapshot", snapshotID)
	output.Field("Primary", primary)
	output.Field("Repository", cfg.BackupsPath)

	// Verify primary is running and ready
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, primary, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("primary pod %s not found: %w", primary, err)
	}
	if !k8s.PodReady(*pod, "postgres") {
		return nil, fmt.Errorf("primary pod %s is not running and ready", primary)
	}

	// Get replica instance names (non-primary)
	var replicas []string
	if names := k8s.GetNestedSlice(r.p.Cluster(), "status", "instanceNames"); names != nil {
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
			ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to fence replicas: %w", err)
		}
	}

	// Set up pipe: restic dump -> pipe -> psql stdin
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
		r.unfenceAll(ctx, ns)
		return nil, fmt.Errorf("restore stream failed: %w", err)
	}
	if resticErr != nil {
		r.unfenceAll(ctx, ns)
		return nil, fmt.Errorf("restic dump failed: %w", resticErr)
	}

	output.Section("Restore Complete")

	// Unfence replicas
	if len(replicas) > 0 {
		r.unfenceAll(ctx, ns)

		// Delete replica pods to force clean re-sync
		for _, replica := range replicas {
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, replica, metav1.DeleteOptions{
				GracePeriodSeconds: common.Ptr(int64(0)),
			})
		}
		common.InfoLog("Replicas unfenced and deleted — they will re-sync from primary via streaming replication")
	}

	output.Success("Restore complete")
	return &model.RestoreResult{
		Engine:     r.p.Name(),
		Cluster:    model.ObjectRef{Namespace: ns, Name: cfg.ClusterName},
		SnapshotID: snapshotID,
		Duration:   time.Since(start),
	}, nil
}

func (r *cnpgRestore) unfenceAll(ctx context.Context, ns string) {
	cfg := r.p.Config()
	c := k8s.GetClients()
	patch := `{"metadata":{"annotations":{"cnpg.io/fencedInstances":"[]"}}}`
	_, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		common.WarnLog("Failed to unfence replicas: %v", err)
	}
}

// ptr returns a pointer to the given value.
