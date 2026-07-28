package backup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/restic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	Register("cnpg", func(p provider.EngineProvider) (Backer, error) {
		cp, ok := p.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("cnpg backup: expected *provider.CNPGProvider, got %T", p)
		}
		return &cnpgBackup{p: cp}, nil
	})
}

// cnpgDumpFilename is the virtual filename used in restic snapshots for pg_dumpall output.
const cnpgDumpFilename = "pgdumpall.sql"

// cnpgBackup implements Backer for CloudNativePG PostgreSQL clusters.
type cnpgBackup struct {
	p *provider.CNPGProvider
}

func (b *cnpgBackup) Name() string { return "cnpg" }

func (b *cnpgBackup) Backup(ctx context.Context) (*model.BackupResult, error) {
	cfg := b.p.Config()
	if cfg.BackupMethod == "native" {
		return b.backupNative(ctx)
	}
	primary := k8s.GetNestedString(b.p.Cluster(), "status", "currentPrimary")
	ns := cfg.Namespace
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, cfg.ClusterName, cnpgDumpFilename)
	return b.BackupDump(ctx, "backup", primary, stdinFilename, time.Now(), nil)
}

// newResticClient creates a restic client from the current config.
func (b *cnpgBackup) newResticClient() *restic.Client {
	cfg := b.p.Config()
	return restic.NewClient(cfg.BackupsPath, cfg.ResticPassword)
}

// BackupDump streams pg_dumpall from a donor pod through restic backup --stdin.
// backupType sets the type tag (backup or diverged).
// donor is the pod to dump from. stdinFilename is the virtual path in the snapshot.
// jobTime is set as the restic snapshot timestamp via --time.
// extraTags are merged into the snapshot tags (e.g., job=<id> for diverged grouping).
func (b *cnpgBackup) BackupDump(ctx context.Context, backupType, donor, stdinFilename string, jobTime time.Time, extraTags map[string]string) (*model.BackupResult, error) {
	start := time.Now()
	cfg := b.p.Config()
	ns := cfg.Namespace

	output.Section("Dump Backup")
	output.Field("Type", backupType)
	output.Field("Donor", donor)
	output.Field("Repository", cfg.BackupsPath)

	// Verify donor is running and ready
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, donor, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("donor pod %s not found: %w", donor, err)
	}
	if !k8s.PodReady(*pod, "postgres") {
		return nil, fmt.Errorf("donor pod %s is not running and ready", donor)
	}

	if cfg.DryRun {
		output.Info("DRY RUN: would dump %s from %s and write a %q snapshot to %s. No changes made.",
			stdinFilename, donor, backupType, cfg.BackupsPath)
		return &model.BackupResult{Engine: b.Name(), Cluster: model.ObjectRef{Namespace: ns, Name: cfg.ClusterName},
			Repository: cfg.BackupsPath, Duration: time.Since(start)}, nil
	}

	// Initialize restic repo (idempotent)
	rc := b.newResticClient()
	if err := rc.Init(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize restic repository: %w", err)
	}

	// Set up pipe: pg_dumpall stdout → restic backup stdin
	reader, wait := k8s.ExecPipeOut(ctx, donor, ns, "postgres",
		[]string{"pg_dumpall", "-U", "postgres"})

	tags := map[string]string{
		"engine":    "cnpg",
		"cluster":   cfg.ClusterName,
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

	result := &model.BackupResult{
		Engine:     b.Name(),
		Cluster:    model.ObjectRef{Namespace: ns, Name: cfg.ClusterName},
		SnapshotID: summary.SnapshotID,
		Repository: cfg.BackupsPath,
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

func (b *cnpgBackup) backupNative(ctx context.Context) (*model.BackupResult, error) {
	start := time.Now()
	cfg := b.p.Config()

	// Verify barmanObjectStore is configured
	backup := k8s.GetNestedMap(b.p.Cluster(), "spec", "backup")
	if backup == nil {
		return nil, fmt.Errorf("barmanObjectStore not configured on cluster '%s'. Use --method dump or configure S3 backup", cfg.ClusterName)
	}
	if _, ok := backup["barmanObjectStore"]; !ok {
		return nil, fmt.Errorf("barmanObjectStore not configured on cluster '%s'", cfg.ClusterName)
	}

	backupName := fmt.Sprintf("%s-%s", cfg.ClusterName, strings.ToLower(time.Now().Format("20060102t150405")))

	output.Section("Native S3 Backup")
	output.Field("Backup CR", backupName)
	output.Field("Cluster", cfg.ClusterName)
	output.Field("Method", "barmanObjectStore")

	if cfg.DryRun {
		output.Info("DRY RUN: would create Backup CR %s (method barmanObjectStore) for cluster %s. No changes made.",
			backupName, cfg.ClusterName)
		return &model.BackupResult{Engine: b.Name(), Cluster: model.ObjectRef{Namespace: cfg.Namespace, Name: cfg.ClusterName},
			Duration: time.Since(start)}, nil
	}

	// Create Backup CRD
	c := k8s.GetClients()
	backupObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "postgresql.cnpg.io/v1",
			"kind":       "Backup",
			"metadata": map[string]interface{}{
				"name":      backupName,
				"namespace": cfg.Namespace,
			},
			"spec": map[string]interface{}{
				"cluster": map[string]interface{}{
					"name": cfg.ClusterName,
				},
				"method": "barmanObjectStore",
			},
		},
	}

	gvr := schema.GroupVersionResource{
		Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups",
	}
	_, err := c.Dynamic.Resource(gvr).Namespace(cfg.Namespace).Create(ctx, backupObj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create Backup CRD: %w", err)
	}

	// Wait for completion (120 retries * 10s = 20 min max)
	common.InfoLog("Waiting for backup %s to complete...", backupName)
	for i := 0; i < 120; i++ {
		time.Sleep(10 * time.Second)
		obj, err := c.Dynamic.Resource(gvr).Namespace(cfg.Namespace).Get(ctx, backupName, metav1.GetOptions{})
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
			return &model.BackupResult{
				Engine:     b.Name(),
				Cluster:    model.ObjectRef{Namespace: cfg.Namespace, Name: cfg.ClusterName},
				SnapshotID: backupName,
				Repository: dest,
				Duration:   time.Since(start),
				Tags: map[string]string{
					"engine":  "cnpg",
					"cluster": cfg.ClusterName,
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
