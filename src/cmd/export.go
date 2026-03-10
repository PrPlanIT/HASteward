package cmd

import (
	"compress/gzip"
	"fmt"
	"os"
	"strconv"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/restic"

	"github.com/spf13/cobra"
)

var exportOutput string

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Extract a backup snapshot to a local .sql.gz file",
	Long: `Exports a database dump from a restic snapshot to a local gzipped SQL file.

For diverged snapshots, use -i to specify the instance ordinal.

Examples:
  hasteward export -e cnpg -c zitadel-postgres -n zeldas-lullaby --snapshot latest -o dump.sql.gz
  hasteward export -e cnpg -c zitadel-postgres -n zeldas-lullaby --snapshot abc123 -i 2 -o instance2.sql.gz`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if Cfg.BackupsPath == "" {
			return fmt.Errorf("export requires --backups-path")
		}
		if Cfg.ResticPassword == "" {
			return fmt.Errorf("export requires RESTIC_PASSWORD env var")
		}
		if exportOutput == "" {
			return fmt.Errorf("export requires --output/-o")
		}
		if Cfg.Engine == "" {
			return fmt.Errorf("export requires --engine/-e")
		}
		if Cfg.ClusterName == "" {
			return fmt.Errorf("export requires --cluster/-c")
		}
		if Cfg.Namespace == "" {
			return fmt.Errorf("export requires --namespace/-n")
		}

		if Cfg.Verbose {
			os.Setenv(common.EnvPrefix+"LOG_LEVEL", "debug")
			common.InitLogging(false)
		}
		if Cfg.ResticPassword != "" {
			common.RegisterSecret(Cfg.ResticPassword)
		}

		if err := ResolveInstance(cmd); err != nil {
			return err
		}

		rc := restic.NewClient(Cfg.BackupsPath, Cfg.ResticPassword)

		var dumpFile string
		switch Cfg.Engine {
		case "cnpg":
			dumpFile = "pgdumpall.sql"
		case "galera":
			dumpFile = "mysqldump.sql"
		default:
			return fmt.Errorf("unknown engine %q", Cfg.Engine)
		}

		if Cfg.InstanceNumber != nil {
			dumpFile = strconv.Itoa(*Cfg.InstanceNumber) + "-" + dumpFile
		}
		snapshotPath := Cfg.Namespace + "/" + Cfg.ClusterName + "/" + dumpFile

		tags := map[string]string{
			"engine":    Cfg.Engine,
			"cluster":   Cfg.ClusterName,
			"namespace": Cfg.Namespace,
		}

		f, err := os.Create(exportOutput)
		if err != nil {
			return fmt.Errorf("failed to create output file: %w", err)
		}
		defer f.Close()

		gz := gzip.NewWriter(f)
		defer gz.Close()

		snapshot := Cfg.Snapshot
		if snapshot == "" {
			snapshot = "latest"
		}

		common.InfoLog("Exporting snapshot %s path %s to %s", snapshot, snapshotPath, exportOutput)
		if err := rc.Dump(cmd.Context(), snapshot, snapshotPath, gz, tags); err != nil {
			gz.Close()
			f.Close()
			os.Remove(exportOutput)
			return fmt.Errorf("export failed: %w", err)
		}

		common.InfoLog("Export complete: %s", exportOutput)
		return nil
	},
}

func init() {
	exportCmd.Flags().StringVarP(&exportOutput, "output", "o", "", "Output file path (e.g., dump.sql.gz)")
}
