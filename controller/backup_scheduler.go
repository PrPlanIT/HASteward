package controller

import (
	"context"
	"log/slog"

	v1alpha1 "gitlab.prplanit.com/precisionplanit/hasteward/api/v1alpha1"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/metrics"
)

// runBackup is called by the cron scheduler to back up a database to a specific repository.
func (s *Scheduler) runBackup(db *ManagedDB, repoName string) {
	ctx := context.Background()
	log := slog.With("engine", db.Engine, "cluster", db.ClusterName, "namespace", db.Namespace, "repository", repoName)

	log.Info("Starting scheduled backup")

	// Fetch repository credentials
	repository, password, envVars, err := s.getRepoCredentials(ctx, repoName)
	if err != nil {
		log.Error("Failed to get repository credentials", "error", err)
		return
	}
	common.RegisterSecret(password)

	// Build engine config
	cfg := &common.Config{
		Engine:         db.Engine,
		ClusterName:    db.ClusterName,
		Namespace:      db.Namespace,
		Mode:           "backup",
		BackupsPath:    repository,
		ResticPassword: password,
		BackupMethod:   "dump",
		HealTimeout:    db.Config.HealTimeout,
		DeleteTimeout:  db.Config.DeleteTimeout,
	}

	// Set restic env vars (S3 credentials)
	_ = envVars // TODO: pass env vars to restic client when Config supports it

	// Get and validate engine
	eng, err := engine.Get(cfg.Engine)
	if err != nil {
		log.Error("Engine not found", "error", err)
		return
	}
	if err := eng.Validate(ctx, cfg); err != nil {
		log.Error("Engine validation failed", "error", err)
		return
	}

	// Run backup
	result, err := eng.Backup(ctx)
	if err != nil {
		log.Error("Backup failed", "error", err)
		metrics.RecordBackupFailure(db.Engine, db.ClusterName, db.Namespace, repoName)
		s.updateStatusAnnotation(ctx, db, v1alpha1.AnnotationLastBackup, nowRFC3339())
		return
	}

	log.Info("Backup completed",
		"snapshot", result.SnapshotID,
		"size", result.Size,
		"duration", result.Duration.String())

	metrics.RecordBackupSuccess(db.Engine, db.ClusterName, db.Namespace, repoName, result)

	// Update status annotations
	s.updateMultipleAnnotations(ctx, db, map[string]string{
		v1alpha1.AnnotationLastBackup:         nowRFC3339(),
		v1alpha1.AnnotationLastBackupDuration: result.Duration.Truncate(1e9).String(), // truncate to seconds
	})
}
