package cmd

import (
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/engine/backup"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/output/printer"

	"github.com/spf13/cobra"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Manage cluster backups (create, list, restore, export, prune, policies, repositories)",
	Long:  "Backup lifecycle for managed clusters. Use 'backup create' to take a backup.",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := InitPrinter("backup")
		if err != nil {
			return err
		}

		if Cfg.BackupMethod != "native" {
			if Cfg.BackupsPath == "" {
				return fmt.Errorf("backup requires --backups-path (or --method native for CNPG S3)")
			}
			if Cfg.ResticPassword == "" {
				return fmt.Errorf("backup requires RESTIC_PASSWORD env var")
			}
		}

		prov, err := PreRun(cmd, "backup")
		if err != nil {
			return err
		}

		backer, err := backup.Get(prov)
		if err != nil {
			return err
		}

		result, err := backup.Run(cmd.Context(), backer, newSink(p))
		if err != nil {
			if !p.IsHuman() {
				printer.PrintResult(p, (*model.BackupResult)(nil), nil, err)
			}
			return err
		}

		if p.IsHuman() {
			output.Complete(fmt.Sprintf("Backup complete — snapshot %s (%s)", result.SnapshotID, result.Duration.Truncate(time.Second)))
		} else {
			printer.PrintResult(p, result, nil, nil)
		}
		return nil
	},
}
