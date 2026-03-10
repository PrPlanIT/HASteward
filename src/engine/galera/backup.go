package galera

import (
	"context"
	"fmt"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/restic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DumpFilename is the virtual filename used in restic snapshots for mysqldump output.
const DumpFilename = "mysqldump.sql"

func (e *Engine) Backup(ctx context.Context) (*common.BackupResult, error) {
	ns := e.cfg.Namespace
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, e.cfg.ClusterName, DumpFilename)
	return e.BackupDump(ctx, "backup", "", stdinFilename, time.Now(), nil)
}

// newResticClient creates a restic client from the current config.
func (e *Engine) newResticClient() *restic.Client {
	return restic.NewClient(e.cfg.BackupsPath, e.cfg.ResticPassword)
}

// BackupDump streams mysqldump from a donor pod through restic backup --stdin.
// backupType sets the type tag (backup or diverged).
// donor is the pod to dump from; if empty, finds a healthy running pod.
// stdinFilename is the virtual path in the snapshot.
// jobTime is set as the restic snapshot timestamp via --time.
// extraTags are merged into the snapshot tags (e.g., job=<id> for diverged grouping).
func (e *Engine) BackupDump(ctx context.Context, backupType, donor, stdinFilename string, jobTime time.Time, extraTags map[string]string) (*common.BackupResult, error) {
	start := time.Now()
	ns := e.cfg.Namespace

	// Find donor if not specified
	if donor == "" {
		var err error
		donor, err = e.findHealthyPod(ctx)
		if err != nil {
			return nil, err
		}
	}

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

	// Stream backup using MYSQL_PWD env var (security: not in command args)
	// MariaDB 12+ renamed mysqldump to mariadb-dump; try both with fallback
	cmd := []string{"sh", "-c",
		"export MYSQL_PWD='" + k8s.ShellEscape(e.rootPassword) + "'; " +
			"DUMPCMD=$(command -v mariadb-dump 2>/dev/null || command -v mysqldump 2>/dev/null) && " +
			"$DUMPCMD -u root --all-databases --single-transaction --routines --triggers --events"}

	// Set up pipe: mysqldump stdout → restic backup stdin
	reader, wait := k8s.ExecPipeOut(ctx, donor, ns, "mariadb", cmd)

	tags := map[string]string{
		"engine":    "galera",
		"cluster":   e.cfg.ClusterName,
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

// findHealthyPod returns the name of a healthy running MariaDB pod.
func (e *Engine) findHealthyPod(ctx context.Context) (string, error) {
	c := k8s.GetClients()
	pods, err := c.Clientset.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + e.cfg.ClusterName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == "Running" && len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
			return pod.Name, nil
		}
	}

	return "", fmt.Errorf("no healthy running pods found for %s in %s", e.cfg.ClusterName, e.cfg.Namespace)
}
