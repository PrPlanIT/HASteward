package cmd

import (
	"os"

	"github.com/PrPlanIT/HASteward/controller"
	"github.com/PrPlanIT/HASteward/src/common"

	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the hasteward operator (controller + scheduler)",
	Long: `Starts the hasteward operator which watches CNPG Cluster and MariaDB CRs
for clinic.hasteward.prplanit.com/policy annotations and automatically runs scheduled backups
and triage/repair operations based on BackupPolicy configuration.

Endpoints:
  :8080/metrics   Prometheus metrics
  :8081/healthz   Liveness probe
  :8081/readyz    Readiness probe`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if Cfg.Verbose {
			os.Setenv(common.EnvPrefix+"LOG_LEVEL", "debug")
		}
		common.InitLogging(true)

		return controller.Run(cmd.Context(), Cfg.Kubeconfig)
	},
}
