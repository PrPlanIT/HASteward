package cmd

import (
	"fmt"
	"time"

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

-t escrow releases escrow VolumeSnapshots instead: the rollback points taken
before a split-brain repair or an in-place WAL replay. It is deliberately NOT
part of -t all. An escrow is the rollback for a change already made, and on a
snapshot class with deletionPolicy Delete, releasing the object destroys the
backing storage snapshot. Retention groups escrows by the run that took them,
keeps the most recent --keep-last runs, and never releases one younger than
--escrow-min-age whatever the count says. Snapshots this tool did not label are
reported and left alone.

For diverged snapshots, retention is group-aware: snapshots sharing the same
job tag (from one repair operation) are kept or removed as a unit. So
--keep-last 3 means "keep the 3 most recent repair jobs" regardless of how
many instances each job captured.

Examples:
  hasteward backup prune -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups
  hasteward backup prune -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups \
    --keep-last 7 --keep-daily 30 --keep-weekly 12 --keep-monthly 24
  hasteward backup prune -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups \
    -t diverged --keep-last 3
  hasteward backup prune -e cnpg -c gatus-postgres -n gossip-stone \
    -t escrow --keep-last 2 --escrow-min-age 168h --dry-run`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := InitPrinter("prune-backups")
		if err != nil {
			return err
		}

		switch pbType {
		case "backup", "diverged", "all":
			// Restic-backed types need the repo; escrow does not.
			if Cfg.BackupsPath == "" {
				return fmt.Errorf("prune backups requires --backups-path")
			}
			if Cfg.ResticPassword == "" {
				return fmt.Errorf("prune backups requires RESTIC_PASSWORD env var")
			}
		case "escrow":
			if pbEscrowMinAge <= 0 {
				return fmt.Errorf("--escrow-min-age must be positive (got %s)", pbEscrowMinAge)
			}
		default:
			return fmt.Errorf("--type must be backup, diverged, escrow, or all (got %q)", pbType)
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

			EscrowMinAge: pbEscrowMinAge,
		}

		result, err := retention.Run(cmd.Context(), retainer, opts, newSink(p))
		if err != nil {
			if !p.IsHuman() {
				printer.PrintResult(p, (*model.PruneResult)(nil), nil, err)
			}
			return err
		}

		if p.IsHuman() {
			noun := "snapshots"
			verb := "Pruned"
			if pbType == "escrow" {
				noun = "escrow snapshots"
				verb = "Released"
			}
			output.Complete(fmt.Sprintf("%s %d %s, kept %d", verb, result.TotalRemoved, noun, result.TotalKept))
		} else {
			printer.PrintResult(p, result, nil, nil)
		}
		return nil
	},
}

var (
	pbKeepLast     int
	pbKeepDaily    int
	pbKeepWeekly   int
	pbKeepMonthly  int
	pbType         string
	pbEscrowMinAge time.Duration
)

func init() {
	pruneBackupsCmd.Flags().IntVar(&pbKeepLast, "keep-last", 7, "Keep the last N snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepDaily, "keep-daily", 30, "Keep N daily snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepWeekly, "keep-weekly", 12, "Keep N weekly snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepMonthly, "keep-monthly", 24, "Keep N monthly snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().StringVarP(&pbType, "type", "t", "backup", "Snapshot type to prune: backup, diverged, escrow, or all")
	pruneBackupsCmd.Flags().DurationVar(&pbEscrowMinAge, "escrow-min-age", 7*24*time.Hour,
		"Never release an escrow younger than this, whatever --keep-last says (-t escrow only)")
}
