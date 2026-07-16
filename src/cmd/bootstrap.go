package cmd

import (
	"fmt"

	"github.com/PrPlanIT/HASteward/src/engine/bootstrap"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/printer"

	"github.com/spf13/cobra"
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Bootstrap a Galera cluster by declaring which node the operator bootstraps from",
	Long: `Bootstrap a Galera cluster by declaring which node the operator should bootstrap
from. There are two situations, one goal (force the operator to bootstrap the
authoritative node); bootstrap picks the path from the cluster's state:

  ALL NODES DOWN (offline) — DANGEROUS. Identifies the highest-seqno node, establishes
  authority offline (scale to 0 -> wsrep_recover), sets safe_to_bootstrap=1, patches the
  CR with forceClusterBootstrapInPod, and brings the cluster back from total failure.

  OPERATOR RECOVERY DEADLOCK (online) — when triage diagnoses
  'galera-operator-recovery-deadlock' (the operator is stuck in a recovery it cannot
  resolve because no node yields a seqno, while the DATA is healthy), force-bootstraps the
  already-synced authority on the LIVE cluster (no scale-to-0), deletes the stuck recovery
  jobs, and lets the operator reform. Non-destructive.

Safety gates:
  - Refuses on a healthy cluster UNLESS triage diagnosed a recovery deadlock (else use 'repair')
  - Refuses if seqno is ambiguous across nodes (unless --force)
  - Refuses if split-brain is detected (unless --force)
  - Supports --dry-run to preview the plan without mutation

Use --dry-run --output json for automation to inspect the decision
before approving execution.

Examples:
  hasteward bootstrap -e galera -c kimai-mariadb -n hyrule-castle
  hasteward bootstrap -e galera -c kimai-mariadb -n hyrule-castle --dry-run
  hasteward bootstrap -e galera -c kimai-mariadb -n hyrule-castle --dry-run --output json
  hasteward bootstrap -e galera -c kimai-mariadb -n hyrule-castle --force`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := InitPrinter("bootstrap")
		if err != nil {
			return err
		}

		prov, err := PreRun(cmd, "bootstrap")
		if err != nil {
			return err
		}

		bootstrapper, err := bootstrap.Get(prov)
		if err != nil {
			return err
		}

		result, err := bootstrap.Run(cmd.Context(), bootstrapper, IsDryRun(), newSink(p))
		if err != nil {
			if !p.IsHuman() && result != nil {
				// Return the partial result (includes decision) even on error
				printer.PrintResult(p, result, nil, err)
			}
			return err
		}

		if p.IsHuman() {
			if IsDryRun() {
				output.Banner("DRY RUN — Bootstrap Plan")
				output.Field("Candidate", result.Decision.CandidatePod)
				output.Field("Seqno", fmt.Sprintf("%d", result.Decision.CandidateSeqno))
				output.Field("UUID", result.Decision.CandidateUUID)
				if result.Decision.AmbiguityDetected {
					output.Warn("Ambiguity detected: competitors %v", result.Decision.Competitors)
				}
				output.Section("Planned Actions")
				for _, action := range result.ActionsPlanned {
					output.Bullet(0, "[%s] %s", action.Phase, action.Description)
				}
				output.Info("Re-run without --dry-run to execute")
			} else {
				output.Complete("Bootstrap complete")
			}
		} else {
			printer.PrintResult(p, result, nil, nil)
		}
		return nil
	},
}

func init() {
	RootCmd.AddCommand(bootstrapCmd)
}
