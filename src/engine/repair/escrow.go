package repair

import (
	"context"
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/backup"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// runEscrow is the shared pre-repair escrow for both engines: a full backup from
// donorPod, plus — on a split-brain (!SafeToHeal) — a diverged per-instance backup
// of every running/ready node so no lineage's data is lost before healing. The
// engines differ only in the donor source (galera: the resolved donor; cnpg:
// currentPrimary) and the dump filename. donorPod == "" skips the pre-repair
// backup (no donor resolved). Diverged backups are best-effort: a failure on one
// node is logged and the rest continue.
func runEscrow(ctx context.Context, cfg *common.Config, backuper backup.Backer, result *model.TriageResult, donorPod, dumpFilename string) error {
	start := time.Now()

	if !cfg.NoEscrow {
		if cfg.BackupsPath == "" || cfg.ResticPassword == "" {
			return fmt.Errorf("repair requires --backups-path and RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
		}
		if donorPod != "" {
			stdinFilename := fmt.Sprintf("%s/%s/%s", cfg.Namespace, cfg.ClusterName, dumpFilename)
			escrowResult, err := backuper.BackupDump(ctx, "backup", donorPod, stdinFilename, start, nil)
			if err != nil {
				return fmt.Errorf("pre-repair backup failed: %w", err)
			}
			common.InfoLog("Pre-repair backup from %s: %s", donorPod, escrowResult.SnapshotID)
		} else {
			common.WarnLog("No donor resolved for pre-repair backup. Skipping.")
		}
	} else {
		common.WarnLog("no_escrow=true — proceeding without pre-repair backup")
	}

	// Diverged per-instance backups (when split-brain detected).
	if !result.DataComparison.SafeToHeal && !cfg.NoEscrow {
		jobID := start.UTC().Format("20060102T150405Z")
		common.WarnLog("Split-brain detected — capturing per-instance diverged backups (job=%s)", jobID)
		for _, a := range result.Assessments {
			if !a.IsRunning || !a.IsReady {
				common.WarnLog("Skipping diverged backup for %s (not running/ready)", a.Pod)
				continue
			}
			stdinFilename := fmt.Sprintf("%s/%s/%d-%s", cfg.Namespace, cfg.ClusterName, a.Instance, dumpFilename)
			extraTags := map[string]string{"job": jobID}
			divResult, err := backuper.BackupDump(ctx, "diverged", a.Pod, stdinFilename, start, extraTags)
			if err != nil {
				common.WarnLog("Failed diverged backup for %s: %v", a.Pod, err)
				continue
			}
			common.InfoLog("Diverged backup %s: %s", a.Pod, divResult.SnapshotID)
		}
	}

	return nil
}
