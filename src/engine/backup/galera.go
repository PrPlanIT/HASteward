package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/restic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	Register("galera", func(p provider.EngineProvider) (Backer, error) {
		gp, ok := p.(*provider.GaleraProvider)
		if !ok {
			return nil, fmt.Errorf("galera backup: expected *provider.GaleraProvider, got %T", p)
		}
		return &galeraBackup{p: gp}, nil
	})
}

// DumpFilename is the virtual filename used in restic snapshots for mysqldump output.
const galeraDumpFilename = "mysqldump.sql"

// galeraBackup implements Backer for MariaDB Galera clusters.
type galeraBackup struct {
	p *provider.GaleraProvider
}

func (b *galeraBackup) Name() string { return "galera" }

func (b *galeraBackup) Backup(ctx context.Context) (*model.BackupResult, error) {
	cfg := b.p.Config()
	ns := cfg.Namespace
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, cfg.ClusterName, galeraDumpFilename)
	return b.BackupDump(ctx, "backup", "", stdinFilename, time.Now(), nil)
}

// newResticClient creates a restic client from the current config.
func (b *galeraBackup) newResticClient() *restic.Client {
	cfg := b.p.Config()
	return restic.NewClient(cfg.BackupsPath, cfg.ResticPassword)
}

// BackupDump streams mysqldump from a donor pod through restic backup --stdin.
// backupType sets the type tag (backup or diverged).
// donor is the pod to dump from; if empty, finds a healthy running pod.
// stdinFilename is the virtual path in the snapshot.
// jobTime is set as the restic snapshot timestamp via --time.
// extraTags are merged into the snapshot tags (e.g., job=<id> for diverged grouping).
func (b *galeraBackup) BackupDump(ctx context.Context, backupType, donor, stdinFilename string, jobTime time.Time, extraTags map[string]string) (*model.BackupResult, error) {
	start := time.Now()
	cfg := b.p.Config()
	ns := cfg.Namespace

	// Find donor if not specified
	if donor == "" {
		var err error
		donor, err = b.findHealthyPod(ctx)
		if err != nil {
			return nil, err
		}
	}

	output.Section("Dump Backup")
	output.Field("Type", backupType)
	output.Field("Donor", donor)
	output.Field("Repository", cfg.BackupsPath)

	// Verify donor pod exists and is Running.
	// K8s container readiness is NOT checked here — the donor was already
	// validated upstream via Galera wsrep probe. Re-gating on K8s readiness
	// would reintroduce the abstraction leak this design explicitly removes.
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, donor, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("donor pod %s not found: %w", donor, err)
	}
	if pod.Status.Phase != "Running" {
		return nil, fmt.Errorf("donor pod %s is not Running (phase: %s)", donor, pod.Status.Phase)
	}

	// Initialize restic repo (idempotent)
	rc := b.newResticClient()
	if err := rc.Init(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize restic repository: %w", err)
	}

	// Stream backup using MYSQL_PWD env var (security: not in command args)
	// MariaDB 12+ renamed mysqldump to mariadb-dump; try both with fallback
	cmd := []string{"sh", "-c",
		"export MYSQL_PWD='" + k8s.ShellEscape(b.p.RootPassword()) + "'; " +
			"DUMPCMD=$(command -v mariadb-dump 2>/dev/null || command -v mysqldump 2>/dev/null) && " +
			"$DUMPCMD -u root --all-databases --single-transaction --routines --triggers --events"}

	// Set up pipe: mysqldump stdout → restic backup stdin
	reader, wait := k8s.ExecPipeOut(ctx, donor, ns, "mariadb", cmd)

	tags := map[string]string{
		"engine":    "galera",
		"cluster":   cfg.ClusterName,
		"namespace": ns,
		"type":      backupType,
	}
	for k, v := range extraTags {
		tags[k] = v
	}

	common.InfoLog("Streaming mysqldump → restic backup --stdin")
	summary, err := rc.BackupStdin(ctx, reader, stdinFilename, tags, jobTime)

	// Wait for k8s exec to finish
	execErr := wait()

	if err != nil {
		return nil, fmt.Errorf("restic backup failed: %w", err)
	}
	if execErr != nil {
		return nil, fmt.Errorf("mysqldump exec failed: %w", execErr)
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

// findHealthyPod returns the name of a healthy running MariaDB pod.
func (b *galeraBackup) findHealthyPod(ctx context.Context) (string, error) {
	cfg := b.p.Config()
	return k8s.FindReadyPod(ctx, cfg.Namespace, b.p.PodSelector(), "mariadb")
}
