package cnpg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/restic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DumpFilename is the virtual filename used in restic snapshots for pg_dumpall output.
const DumpFilename = "pgdumpall.sql"

func (e *Engine) Backup(ctx context.Context) (*common.BackupResult, error) {
	if e.cfg.BackupMethod == "native" {
		return e.backupNative(ctx)
	}
	primary := k8s.GetNestedString(e.cluster, "status", "currentPrimary")
	ns := e.cfg.Namespace
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, e.cfg.ClusterName, DumpFilename)
	return e.BackupDump(ctx, "backup", primary, stdinFilename, time.Now(), nil)
}

// newResticClient creates a restic client from the current config.
func (e *Engine) newResticClient() *restic.Client {
	return restic.NewClient(e.cfg.BackupsPath, e.cfg.ResticPassword)
}

// BackupDump streams pg_dumpall from a donor pod through restic backup --stdin.
// backupType sets the type tag (backup or diverged).
// donor is the pod to dump from. stdinFilename is the virtual path in the snapshot.
// jobTime is set as the restic snapshot timestamp via --time.
// extraTags are merged into the snapshot tags (e.g., job=<id> for diverged grouping).
func (e *Engine) BackupDump(ctx context.Context, backupType, donor, stdinFilename string, jobTime time.Time, extraTags map[string]string) (*common.BackupResult, error) {
	start := time.Now()
	ns := e.cfg.Namespace

	output.Section("Dump Backup")
	output.Field("Type", backupType)
	output.Field("Donor", donor)
	output.Field("Repository", e.cfg.BackupsPath)

	// Verify donor is running and ready
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, donor, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("donor pod %s not found: %w", donor, err)
	}
	if pod.Status.Phase != "Running" || len(pod.Status.ContainerStatuses) == 0 || !pod.Status.ContainerStatuses[0].Ready {
		return nil, fmt.Errorf("donor pod %s is not running and ready", donor)
	}

	// Initialize restic repo (idempotent)
	rc := e.newResticClient()
	if err := rc.Init(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize restic repository: %w", err)
	}

	// Set up pipe: pg_dumpall stdout → restic backup stdin
	reader, wait := k8s.ExecPipeOut(ctx, donor, ns, "postgres",
		[]string{"pg_dumpall", "-U", "postgres"})

	tags := map[string]string{
		"engine":    "cnpg",
		"cluster":   e.cfg.ClusterName,
		"namespace": ns,
		"type":      backupType,
	}
	for k, v := range extraTags {
		tags[k] = v
	}

	common.InfoLog("Streaming pg_dumpall → restic backup --stdin")
	summary, err := rc.BackupStdin(ctx, reader, stdinFilename, tags, jobTime)

	// Wait for k8s exec to finish
	execErr := wait()

	if err != nil {
		return nil, fmt.Errorf("restic backup failed: %w", err)
	}
	if execErr != nil {
		return nil, fmt.Errorf("pg_dumpall exec failed: %w", execErr)
	}

	result := &common.BackupResult{
		SnapshotID: summary.SnapshotID,
		Repository: e.cfg.BackupsPath,
		Size:       summary.TotalSize,
		Duration:   time.Since(start),
		Tags:       tags,
	}

	output.Success("Backup snapshot: %s (data added: %s, total: %s, %.1fs)",
		summary.SnapshotID,
		output.FormatBytes(summary.DataAdded),
		output.FormatBytes(summary.TotalSize),
		summary.TotalDuration)
	return result, nil
}

func (e *Engine) backupNative(ctx context.Context) (*common.BackupResult, error) {
	start := time.Now()

	// Verify barmanObjectStore is configured
	backup := k8s.GetNestedMap(e.cluster, "spec", "backup")
	if backup == nil {
		return nil, fmt.Errorf("barmanObjectStore not configured on cluster '%s'. Use --method dump or configure S3 backup", e.cfg.ClusterName)
	}
	if _, ok := backup["barmanObjectStore"]; !ok {
		return nil, fmt.Errorf("barmanObjectStore not configured on cluster '%s'", e.cfg.ClusterName)
	}

	backupName := fmt.Sprintf("%s-%s", e.cfg.ClusterName, strings.ToLower(time.Now().Format("20060102t150405")))

	output.Section("Native S3 Backup")
	output.Field("Backup CR", backupName)
	output.Field("Cluster", e.cfg.ClusterName)
	output.Field("Method", "barmanObjectStore")

	// Create Backup CRD
	c := k8s.GetClients()
	backupObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "postgresql.cnpg.io/v1",
			"kind":       "Backup",
			"metadata": map[string]interface{}{
				"name":      backupName,
				"namespace": e.cfg.Namespace,
			},
			"spec": map[string]interface{}{
				"cluster": map[string]interface{}{
					"name": e.cfg.ClusterName,
				},
				"method": "barmanObjectStore",
			},
		},
	}

	gvr := schema.GroupVersionResource{
		Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups",
	}
	_, err := c.Dynamic.Resource(gvr).Namespace(e.cfg.Namespace).Create(ctx, backupObj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create Backup CRD: %w", err)
	}

	// Wait for completion (120 retries * 10s = 20 min max)
	common.InfoLog("Waiting for backup %s to complete...", backupName)
	for i := 0; i < 120; i++ {
		time.Sleep(10 * time.Second)
		obj, err := c.Dynamic.Resource(gvr).Namespace(e.cfg.Namespace).Get(ctx, backupName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		phase := k8s.GetNestedString(obj, "status", "phase")
		if phase == "completed" {
			dest := k8s.GetNestedString(obj, "status", "destinationPath")
			output.Section("Native Backup Result")
			output.Field("Status", phase)
			output.Field("Destination", dest)
			output.Field("Backup CR", backupName)
			output.Success("Native backup completed")
			return &common.BackupResult{
				SnapshotID: backupName,
				Repository: dest,
				Duration:   time.Since(start),
				Tags: map[string]string{
					"engine":  "cnpg",
					"cluster": e.cfg.ClusterName,
					"method":  "native",
				},
			}, nil
		}
		if phase == "failed" {
			return nil, fmt.Errorf("native backup '%s' failed. Check CNPG Backup CR status for details", backupName)
		}
	}
	return nil, fmt.Errorf("native backup '%s' timed out after 20 minutes", backupName)
}
