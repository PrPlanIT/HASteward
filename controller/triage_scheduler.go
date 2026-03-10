package controller

import (
	"context"
	"log/slog"

	v1alpha1 "gitlab.prplanit.com/precisionplanit/hasteward/api/v1alpha1"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/metrics"
)

// runTriage is called by the cron scheduler to health-check a database.
// If mode=repair and unhealthy instances are found, it triggers auto-repair.
func (s *Scheduler) runTriage(db *ManagedDB) {
	ctx := context.Background()
	log := slog.With("engine", db.Engine, "cluster", db.ClusterName, "namespace", db.Namespace)

	log.Info("Starting scheduled triage")

	// Build engine config (triage doesn't need restic credentials)
	cfg := &common.Config{
		Engine:        db.Engine,
		ClusterName:   db.ClusterName,
		Namespace:     db.Namespace,
		Mode:          "triage",
		HealTimeout:   db.Config.HealTimeout,
		DeleteTimeout: db.Config.DeleteTimeout,
	}

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

	// Run triage
	result, err := eng.Triage(ctx)
	if err != nil {
		log.Error("Triage failed", "error", err)
		return
	}

	// Determine health status
	triageResult := "healthy"
	if result.ReadyCount < result.TotalCount {
		triageResult = "unhealthy"
	}
	if !result.DataComparison.SafeToHeal && len(result.DataComparison.SplitBrainDetails) > 0 {
		triageResult = "split-brain"
	}

	log.Info("Triage completed",
		"ready", result.ReadyCount,
		"total", result.TotalCount,
		"result", triageResult)

	// Record triage metrics (use underscore for split-brain label)
	metricsResult := triageResult
	if metricsResult == "split-brain" {
		metricsResult = "split_brain"
	}
	metrics.RecordTriageResult(db.Engine, db.ClusterName, db.Namespace, result, metricsResult)

	// Update status annotations
	s.updateMultipleAnnotations(ctx, db, map[string]string{
		v1alpha1.AnnotationLastTriage:       nowRFC3339(),
		v1alpha1.AnnotationLastTriageResult: triageResult,
	})

	// Auto-repair if mode=repair and there are unhealthy instances
	if db.Config.Mode == "repair" && triageResult == "unhealthy" {
		s.runAutoRepair(ctx, db, log)
	}
}

// runAutoRepair attempts to repair unhealthy instances after a triage detects problems.
func (s *Scheduler) runAutoRepair(ctx context.Context, db *ManagedDB, log *slog.Logger) {
	log.Info("Auto-repair triggered (mode=repair)")

	// Repair needs restic credentials for escrow backup.
	// Use the first configured repository.
	if len(db.Config.Repositories) == 0 {
		log.Warn("No repositories configured, skipping auto-repair (escrow requires a repository)")
		return
	}

	repoName := db.Config.Repositories[0]
	repository, password, _, err := s.getRepoCredentials(ctx, repoName)
	if err != nil {
		log.Error("Failed to get repository credentials for repair escrow", "repository", repoName, "error", err)
		return
	}
	common.RegisterSecret(password)

	cfg := &common.Config{
		Engine:         db.Engine,
		ClusterName:    db.ClusterName,
		Namespace:      db.Namespace,
		Mode:           "repair",
		BackupsPath:    repository,
		ResticPassword: password,
		BackupMethod:   "dump",
		HealTimeout:    db.Config.HealTimeout,
		DeleteTimeout:  db.Config.DeleteTimeout,
	}

	eng, err := engine.Get(cfg.Engine)
	if err != nil {
		log.Error("Engine not found for repair", "error", err)
		return
	}
	if err := eng.Validate(ctx, cfg); err != nil {
		log.Error("Engine validation failed for repair", "error", err)
		return
	}

	result, err := eng.Repair(ctx)
	if err != nil {
		log.Error("Auto-repair failed", "error", err)
		metrics.RecordRepairFailure(db.Engine, db.ClusterName, db.Namespace)
		s.updateStatusAnnotation(ctx, db, v1alpha1.AnnotationLastRepair, nowRFC3339())
		return
	}

	log.Info("Auto-repair completed",
		"healed", len(result.HealedInstances),
		"skipped", len(result.SkippedInstances),
		"duration", result.Duration.String())

	metrics.RecordRepairSuccess(db.Engine, db.ClusterName, db.Namespace)
	s.updateStatusAnnotation(ctx, db, v1alpha1.AnnotationLastRepair, nowRFC3339())
}
