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
func runEscrow(ctx context.Context, cfg *common.Config, backuper backup.Backer, result *model.TriageResult, donorPod, dumpFilename string) error {
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
				common.WarnLog("Failed diverged backup for %s: %v", a.Pod, err)
				continue
			}
			common.InfoLog("Diverged backup %s: %s", a.Pod, divResult.SnapshotID)
		}
		if err := escrowUndumpable(ctx, cfg, undumpable); err != nil {
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

// selectEscrow is escrow.Select behind a package variable so the fail-closed contract
// below — a refusal or an unproven capture must abort the run — is testable without
// standing up a storage backend. Never reassigned outside tests.
var selectEscrow = escrow.Select

// escrowUndumpable captures the instances a dump could not reach, at the block layer,
// via the same fail-closed provider the deadlock breaker uses — a CSI VolumeSnapshot
// when one matches the PVCs' provisioner, else a restic PVC backup. No postgres is
// required, so a crash-looping instance is captured rather than passed over.
//
// Unlike the per-instance dumps above this is NOT best-effort. Those are redundant
// copies of lineages that are also on a running instance; this is the only copy of
// a lineage that exists nowhere else. Proceeding without it would let a subsequent
// --force destroy the one instance no backup covers, so a failure here stops the run.
//
// CNPG only: an instance's PVC is named after its pod, which is what makes the
// recovery set derivable. Galera does not name them that way, so rather than guess
// at a PVC the gap is reported and left for an operator.
func escrowUndumpable(ctx context.Context, cfg *common.Config, pods []string) error {
	if len(pods) == 0 {
		return nil
	}
	if cfg.Engine != "cnpg" {
		common.WarnLog("Cannot escrow %v: no dump is possible while they are down, and their PVC names are not derivable for the %s engine — capture them manually before any --force", pods, cfg.Engine)
		return nil
	}

	common.WarnLog("Capturing block-level escrow for instances a dump cannot reach: %v", pods)
	prov, err := selectEscrow(ctx, cfg, pods)
	if err != nil {
		return fmt.Errorf("escrow REFUSED for down diverged instance(s) %v: %w", pods, err)
	}
	refs, err := prov.Capture(ctx, pods)
	if err != nil {
		return fmt.Errorf("escrow FAILED for down diverged instance(s) %v: capture: %w", pods, err)
	}
	if err := prov.Verify(ctx, refs); err != nil {
		return fmt.Errorf("escrow FAILED for down diverged instance(s) %v: captured but not proven restorable: %w", pods, err)
	}
	for _, r := range refs {
		common.InfoLog("Diverged escrow %s: %s %s", r.PVC, r.Provider, r.ID)
	}
	return nil
}
