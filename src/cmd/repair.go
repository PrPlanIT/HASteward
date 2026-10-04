package cmd

import (
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/engine/repair"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/output/printer"

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

		// The deadlock breaker's verified escrow IS the rollback that authorizes a
		// destructive datadir clear, so it can never be skipped.
		if Cfg.Unwedge && Cfg.NoEscrow {
			return fmt.Errorf("--unwedge cannot be combined with --no-escrow: the verified escrow is the rollback that authorizes clearing a datadir")
		}
		if Cfg.Promote && Cfg.NoEscrow {
			return fmt.Errorf("--promote cannot be combined with --no-escrow: the verified escrow is the rollback that makes the promotion reversible")
		}
		if Cfg.EscrowOnly {
			if Cfg.NoEscrow {
				return fmt.Errorf("--escrow-only cannot be combined with --no-escrow: capturing the escrow IS the operation")
			}
			// Both of those escrow and then act. --escrow-only is the request to stop
			// after the escrow, so combining them would silently grant the mutation the
			// operator declined to ask for.
			if Cfg.Promote || Cfg.Unwedge {
				return fmt.Errorf("--escrow-only cannot be combined with --promote or --unwedge: it escrows and stops, while those escrow in order to act")
			}
			// Deriving a Galera node's PVC is not possible (they are not named after the
			// pod), so there is nothing to snapshot. Refuse rather than fall through to
			// Galera's inert PreAssess, which would run an ordinary repair.
			if Cfg.Engine != "cnpg" {
				return fmt.Errorf("--escrow-only supports -e cnpg only: a %s node's PVC name is not derivable, so there is nothing to escrow at the block layer", Cfg.Engine)
			}
		}
		// NOTE: --instance is parsed into Cfg.InstanceNumber later, inside PreRun
		// (ResolveInstance) — so it cannot be validated here. promotePrepare enforces
		// "--promote requires --instance" after parsing.

		if !Cfg.NoEscrow {
			if Cfg.BackupsPath == "" {
				return fmt.Errorf("repair requires --backups-path for escrow (or --no-escrow to skip)")
			}
			if Cfg.ResticPassword == "" {
				return fmt.Errorf("repair requires RESTIC_PASSWORD for escrow (or --no-escrow to skip)")
			}
		}

		prov, err := PreRun(cmd, "repair")
		if err != nil {
			return err
		}

		repairer, err := repair.Get(prov)
		if err != nil {
			return err
		}

		result, err := repair.Run(cmd.Context(), repairer, newSink(p))
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
