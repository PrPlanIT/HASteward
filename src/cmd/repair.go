package cmd

import (
	"fmt"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/printer"

	"github.com/spf13/cobra"
)

var repairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Heal unhealthy database instances",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := InitPrinter("repair")
		if err != nil {
			return err
		}

		if !Cfg.NoEscrow {
			if Cfg.BackupsPath == "" {
				return fmt.Errorf("repair requires --backups-path for escrow (or --no-escrow to skip)")
			}
			if Cfg.ResticPassword == "" {
				return fmt.Errorf("repair requires RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
			}
		}

		eng, err := PreRun(cmd, "repair")
		if err != nil {
			return err
		}

		result, err := eng.Repair(cmd.Context())
		if err != nil {
			if !p.IsHuman() {
				printer.PrintResult(p, (*model.RepairResult)(nil), nil, err)
			}
			return err
		}

		if p.IsHuman() {
			summary := fmt.Sprintf("Repair complete — healed: %d, skipped: %d (%s)",
				len(result.HealedInstances), len(result.SkippedInstances), result.Duration.Truncate(time.Second))
			output.Complete(summary)
		} else {
			printer.PrintResult(p, result, nil, nil)
		}
		return nil
	},
}
