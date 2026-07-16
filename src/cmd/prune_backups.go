package cmd

import (
	"fmt"

	"github.com/PrPlanIT/HASteward/src/engine/retention"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/output/printer"

	"github.com/spf13/cobra"
)

var pruneBackupsCmd = &cobra.Command{
	Use:   "prune",
	Short: "Apply retention policy and remove old backup snapshots",
	Long: `Prunes old backup snapshots from restic repositories according to the
configured retention policy (keep-last, keep-daily, keep-weekly, keep-monthly).

By default, only type=backup snapshots are pruned. Use -t diverged to prune
only diverged snapshots, or -t all to prune both types.

For diverged snapshots, retention is group-aware: snapshots sharing the same
job tag (from one repair operation) are kept or removed as a unit. So
--keep-last 3 means "keep the 3 most recent repair jobs" regardless of how
many instances each job captured.

Examples:
  hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups
  hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups \
    --keep-last 7 --keep-daily 30 --keep-weekly 12 --keep-monthly 24
  hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups \
    -t diverged --keep-last 3`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := InitPrinter("prune-backups")
		if err != nil {
			return err
		}

		if Cfg.BackupsPath == "" {
			return fmt.Errorf("prune backups requires --backups-path")
		}
		if Cfg.ResticPassword == "" {
			return fmt.Errorf("prune backups requires RESTIC_PASSWORD env var")
		}

		switch pbType {
		case "backup", "diverged", "all":
		default:
			return fmt.Errorf("--type must be backup, diverged, or all (got %q)", pbType)
		}

		prov, err := PreRun(cmd, "prune backups")
		if err != nil {
			return err
		}

		retainer, err := retention.Get(prov)
		if err != nil {
			return err
		}

		opts := retention.PruneOptions{
			Type:        pbType,
			KeepLast:    pbKeepLast,
			KeepDaily:   pbKeepDaily,
			KeepWeekly:  pbKeepWeekly,
			KeepMonthly: pbKeepMonthly,
		}

		result, err := retention.Run(cmd.Context(), retainer, opts, newSink(p))
		if err != nil {
			if !p.IsHuman() {
				printer.PrintResult(p, (*model.PruneResult)(nil), nil, err)
			}
			return err
		}

		if p.IsHuman() {
			output.Complete(fmt.Sprintf("Pruned %d snapshots, kept %d", result.TotalRemoved, result.TotalKept))
		} else {
			printer.PrintResult(p, result, nil, nil)
		}
		return nil
	},
}

var (
	pbKeepLast    int
	pbKeepDaily   int
	pbKeepWeekly  int
	pbKeepMonthly int
	pbType        string
)

func init() {
	pruneBackupsCmd.Flags().IntVar(&pbKeepLast, "keep-last", 7, "Keep the last N snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepDaily, "keep-daily", 30, "Keep N daily snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepWeekly, "keep-weekly", 12, "Keep N weekly snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepMonthly, "keep-monthly", 24, "Keep N monthly snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().StringVarP(&pbType, "type", "t", "backup", "Snapshot type to prune: backup, diverged, or all")
}
