package cmd

import (
	"fmt"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	"github.com/spf13/cobra"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Back up a database cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		if Cfg.BackupMethod != "native" {
			if Cfg.BackupsPath == "" {
				return fmt.Errorf("backup requires --backups-path (or --method native for CNPG S3)")
			}
			if Cfg.ResticPassword == "" {
				return fmt.Errorf("backup requires RESTIC_PASSWORD env var")
			}
		}

		eng, err := PreRun(cmd, "backup")
		if err != nil {
			return err
		}

		result, err := eng.Backup(cmd.Context())
		if err != nil {
			return err
		}

		output.Complete(fmt.Sprintf("Backup complete — snapshot %s (%s)", result.SnapshotID, result.Duration.Truncate(time.Second)))
		return nil
	},
}
