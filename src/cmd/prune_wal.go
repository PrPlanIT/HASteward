package cmd

import (
	"fmt"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	"github.com/spf13/cobra"
)

var pruneWALCmd = &cobra.Command{
	Use:   "wal",
	Short: "Clear accumulated WAL from a disk-full CNPG instance",
	Long: `Clears accumulated WAL segments from a disk-full PostgreSQL primary.

This is a DESTRUCTIVE operation. It deletes WAL files from the instance's
PVC to free disk space when the primary is stuck in a WAL-accumulation
deadlock (disk full → can't start → replicas can't connect → replication
slots hold WAL → disk stays full).

Safety: Only operates on CNPG clusters. Requires --instance to target a
specific instance. Runs triage first to verify cluster state. Refuses to
proceed if no healthy replicas exist.

Flow: triage → safety check → fence → mount PVC → clear pg_wal → unfence

Examples:
  hasteward prune wal -e cnpg -c nextcloud-postgres -n temple-of-time -i 2
  hasteward prune wal -e cnpg -c grafana-postgres -n gossip-stone -i 1`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if Cfg.Engine != "cnpg" {
			return fmt.Errorf("prune wal is only supported for CNPG clusters (got engine=%q)", Cfg.Engine)
		}

		eng, err := PreRun(cmd, "prune wal")
		if err != nil {
			return err
		}

		if err := eng.PruneWAL(cmd.Context()); err != nil {
			return err
		}

		output.Complete("WAL prune complete")
		return nil
	},
}
