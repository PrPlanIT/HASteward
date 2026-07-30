package cmd

import (
	"github.com/PrPlanIT/HASteward/src/engine/prunewal"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/output/printer"

	"github.com/spf13/cobra"
)

const pruneWALLong = `Clears accumulated WAL segments from a disk-full PostgreSQL primary.

This is a DESTRUCTIVE storage-pressure RECOVERY operation — not backup retention.
It deletes WAL files from the instance's PVC to free disk space when the primary is
stuck in a WAL-accumulation deadlock (disk full -> can't start -> replicas can't
connect -> replication slots hold WAL -> disk stays full).

Safety: Only operates on CNPG clusters. Requires --instance to target a specific
instance. Runs triage first and verifies each ready replica is caught up before it
deletes anything.

Flow: triage -> safety check -> fence -> mount PVC -> clear pg_wal -> unfence

--deadlock-recover: for a disk-full DEADLOCK — an instance too full to start, so it can
never checkpoint to recycle its own (post-checkpoint) WAL. Instead of deleting WAL, it
escrows the PVC (VolumeSnapshot), relocates pg_wal to scratch, replays it single-user +
checkpoints (NO data loss), archivecleans the recycled segments, and moves the small WAL
back — de-bloating the datadir in place on the same volume. Use --dry-run first.

Examples:
  hasteward prune-wal -e cnpg -c nextcloud-postgres -n temple-of-time -i 2
  hasteward prune-wal -e cnpg -c grafana-postgres -n gossip-stone -i 1`

// pruneWALTopCmd is the top-level `prune-wal` — WAL clearing is storage-pressure
// RECOVERY, distinct from the retention `prune` group ("remove retained artifacts").
var pruneWALTopCmd = &cobra.Command{
	Use:   "prune-wal",
	Short: "Clear accumulated WAL from a disk-full CNPG instance (storage-pressure recovery)",
	Long:  pruneWALLong,
	RunE:  runPruneWAL,
}

func init() {
	RootCmd.AddCommand(pruneWALTopCmd)
}

// runPruneWAL clears accumulated WAL from a disk-full CNPG instance. Shared by the
// top-level `prune-wal` and the compat `prune wal`.
func runPruneWAL(cmd *cobra.Command, args []string) error {
	p, err := InitPrinter("prune-wal")
	if err != nil {
		return err
	}

	prov, err := PreRun(cmd, "prune-wal")
	if err != nil {
		return err
	}

	pruner, err := prunewal.Get(prov)
	if err != nil {
		return err
	}

	result, err := prunewal.Run(cmd.Context(), pruner, newSink(p))
	if err != nil {
		if !p.IsHuman() {
			printer.PrintResult(p, (*model.PruneWALResult)(nil), nil, err)
		}
		return err
	}

	if p.IsHuman() {
		output.Complete("WAL prune complete")
	} else {
		printer.PrintResult(p, result, nil, nil)
	}
	return nil
}
