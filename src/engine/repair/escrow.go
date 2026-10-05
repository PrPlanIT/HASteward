package repair

import (
	"context"
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/backup"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/restic"
)

// runEscrow is the shared pre-repair escrow for both engines: a full backup from
// donorPod, plus — on a split-brain (!SafeToHeal) — a diverged per-instance backup
// of every running/ready node so no lineage's data is lost before healing. The
// engines differ only in the donor source (galera: the resolved donor; cnpg:
// currentPrimary) and the dump filename. donorPod == "" skips the pre-repair
// backup (no donor resolved). Diverged backups are best-effort: a failure on one
// node is logged and the rest continue.
func runEscrow(ctx context.Context, cfg *common.Config, namer escrow.PVCNamer, backuper backup.Backer,
	result *model.TriageResult, donorPod, dumpFilename string) error {
	start := time.Now()

	if !cfg.NoEscrow {
		if cfg.BackupsPath == "" || cfg.ResticPassword == "" {
			return fmt.Errorf("repair requires --backups-path and RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
		}
		if err := assertEscrowRepo(ctx, cfg); err != nil {
			return err
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
		var undumpable []string
		for _, a := range result.Assessments {
			if !a.IsRunning || !a.IsReady {
				// A dump needs a live postgres, so this lineage cannot be reached that
				// way. Collected rather than skipped: on a split-brain the instance that
				// is down is frequently the divergent one, which makes it the lineage
				// nothing else holds a copy of.
				undumpable = append(undumpable, a.Pod)
				continue
			}
			stdinFilename := fmt.Sprintf("%s/%s/%d-%s", cfg.Namespace, cfg.ClusterName, a.Instance, dumpFilename)
			extraTags := map[string]string{"job": jobID}
			divResult, err := backuper.BackupDump(ctx, "diverged", a.Pod, stdinFilename, start, extraTags)
			if err != nil {
				// Running is not the same as dumpable, and the reason does not change
				// the consequence: a lineage nobody captured is a lineage that a later
				// --force destroys. Seen on a replica with hot_standby_feedback off,
				// where dumping a large table outlives max_standby_streaming_delay and
				// recovery cancels it — the instance is healthy, the dump simply cannot
				// finish. Fall through to the block layer rather than warn and move on.
				common.WarnLog("Diverged dump failed for %s, falling back to block-level escrow: %v", a.Pod, err)
				undumpable = append(undumpable, a.Pod)
				continue
			}
			common.InfoLog("Diverged backup %s: %s", a.Pod, divResult.SnapshotID)
		}
		if err := escrowUndumpable(ctx, cfg, namer, result, undumpable); err != nil {
			return err
		}
	}

	return nil
}

// escrowRepoExists is the repository presence check behind a package variable, so the
// refusal below is testable without a restic binary or a backend. Never reassigned
// outside tests.
var escrowRepoExists = func(ctx context.Context, cfg *common.Config) (bool, error) {
	return restic.NewClient(cfg.BackupsPath, cfg.ResticPassword).Exists(ctx)
}

// assertEscrowRepo refuses an escrow whose repository does not already exist.
//
// BackupDump initialises one on demand, which is correct for `backup create` — a first
// backup has to start somewhere — and wrong for an escrow. A repository this run just
// created holds nothing and proves nothing. Worse, under the documented container
// wrapper the only mount is the kubeconfig, so an unmounted --backups-path resolves
// inside the container: restic initialises a repository there, the escrow reports a
// snapshot ID, the safety gate is satisfied, repair clears a datadir, and the whole
// repository is discarded when the container exits. The rollback never existed.
//
// So the repository must pre-date the run. Creating one is a deliberate act.
func assertEscrowRepo(ctx context.Context, cfg *common.Config) error {
	ok, err := escrowRepoExists(ctx, cfg)
	if err != nil {
		return fmt.Errorf("escrow REFUSED: cannot open the escrow repository at %s: %w", cfg.BackupsPath, err)
	}
	if !ok {
		return fmt.Errorf("escrow REFUSED: no restic repository at %s — this run would create it, so it would hold nothing and prove nothing. "+
			"Initialise it deliberately (hasteward backup create) and confirm --backups-path names durable storage that is actually mounted: "+
			"under the container wrapper an unmounted path is written inside the container and discarded on exit", cfg.BackupsPath)
	}
	return nil
}

// escrowUndumpable captures the instances a dump could not reach, at the block layer,
// via the same fail-closed provider the deadlock breaker uses — a CSI VolumeSnapshot
// when one matches the PVCs' provisioner, else a restic PVC backup. No postgres is
// required, so a crash-looping instance is captured rather than passed over.
//
// "Could not reach" covers both an instance that is not running and one that is running
// but whose dump failed. The distinction matters to the cause and not at all to the
// outcome: either way that lineage has no copy, and the run is on its way to a --force
// that destroys it.
//
// Unlike the per-instance dumps above this is NOT best-effort. Those are redundant
// copies of lineages that are also on a running instance; this is the only copy of
// a lineage that exists nowhere else. Proceeding without it would let a subsequent
// --force destroy the one instance no backup covers, so a failure here stops the run.
//
// Engine-agnostic: the recovery set is the union of the provider's naming rule and each
// pod's own volume list (DiscoverPVCs), so a Galera node's storage-/galera- pair and a
// CNPG walStorage instance's <pod>-wal are covered as readily as a lone pgdata — and an
// instance whose pod object is already gone still contributes via the naming rule.
func escrowUndumpable(ctx context.Context, cfg *common.Config, namer escrow.PVCNamer,
	result *model.TriageResult, pods []string) error {
	if len(pods) == 0 {
		return nil
	}

	common.WarnLog("Capturing block-level escrow for instances a dump cannot reach: %v", pods)
	// Space is estimated per POD (that is how triage reports disk usage) while the
	// capture is per PVC, so the estimate is a floor — a guard against a full store,
	// not an accounting record.
	pvcs, err := escrow.DiscoverPVCs(ctx, cfg.Namespace, namer, pods)
	if err != nil {
		return fmt.Errorf("escrow of diverged instance(s) %v: %w", pods, err)
	}
	if _, err := escrow.Gate(ctx, cfg, "escrow", pvcs, escrow.UsedBytesByPVC(result, pods)); err != nil {
		return fmt.Errorf("escrow of diverged instance(s) %v: %w", pods, err)
	}
	return nil
}
