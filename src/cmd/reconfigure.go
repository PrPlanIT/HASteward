package cmd

import (
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/engine/reconfigure"
	"github.com/PrPlanIT/HASteward/src/output"

	"github.com/spf13/cobra"
)

var reconfigureCmd = &cobra.Command{
	Use:   "reset-authority",
	Short: "Reset a Galera cluster's authority when its metadata is inconsistent (cluster-wide restart)",
	Long: `Reset the cluster-wide authority on a Galera cluster. Use this when the nodes'
authority metadata is inconsistent — no node can be trusted as the source of truth — and
it must be forcibly corrected. It intentionally stops ALL database pods, rewrites the
authority metadata on the target instance, and restarts the cluster; every node
experiences downtime.

This is NOT repair (instance-scoped data heal) and NOT bootstrap (declaring authority on a
DOWN cluster). Reach for it when the cluster is up but its authority metadata is corrupt.

Requires --force, --instance, and at least one action flag (--fix-bootstrap).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := InitPrinter("reset-authority")
		if err != nil {
			return err
		}

		// Mandatory enforcement BEFORE triage
		if !Cfg.Force {
			return fmt.Errorf("ABORT: reset-authority requires --force (this is a cluster-scoped operation)")
		}
		if !Cfg.FixBootstrap {
			return fmt.Errorf("ABORT: no reset-authority action specified. Use --fix-bootstrap or another action flag")
		}

		// PreRun parses --instance, validates required flags, inits K8s
		prov, err := PreRun(cmd, "reset-authority")
		if err != nil {
			return err
		}

		// Instance check after PreRun (ResolveInstance parses the flag)
		if Cfg.InstanceNumber == nil {
			return fmt.Errorf("ABORT: reset-authority requires --instance (must target a specific node)")
		}
		if *Cfg.InstanceNumber < 0 {
			return fmt.Errorf("ABORT: instance must be non-negative, got %d", *Cfg.InstanceNumber)
		}

		reconf, err := reconfigure.Get(prov)
		if err != nil {
			return err
		}

		ctx := cmd.Context()

		// Phase 1: Triage
		result, err := reconf.Assess(ctx)
		if err != nil {
			return fmt.Errorf("triage failed: %w", err)
		}

		// Phase 2: Validate preconditions
		if err := reconf.Validate(ctx, result); err != nil {
			return err
		}

		// Phase 3: Print plan with warning + blast radius
		reconf.PrintPlan(ctx, result)

		// Phase 4: Execute
		start := time.Now()
		if err := reconf.Execute(ctx, result); err != nil {
			return err
		}

		output.Complete(fmt.Sprintf("Reset-authority complete (%s)", time.Since(start).Truncate(time.Second)))
		return nil
	},
}
