package cmd

import "github.com/spf13/cobra"

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove stale data (backups, WAL)",
	Long: `Prune commands for cleaning up accumulated data.

Available subcommands:
  backups    Apply retention policy and remove old backup snapshots
  wal        Clear accumulated WAL from a disk-full CNPG instance`,
}

func init() {
	pruneCmd.AddCommand(pruneBackupsCmd, pruneWALCmd)
}
