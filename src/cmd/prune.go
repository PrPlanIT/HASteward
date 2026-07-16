package cmd

import "github.com/spf13/cobra"

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Apply retention — remove old backup snapshots",
	Long: `Prune retained artifacts per a retention policy. "Prune" here means only
"remove retained data" — it is not a recovery operation.

Available subcommands:
  backups    Apply retention policy and remove old backup snapshots

Note: clearing WAL from a disk-full instance is storage-pressure RECOVERY, not
retention — it now lives at the top level as 'prune-wal' ('prune wal' still works
as a compat alias).`,
}

func init() {
	pruneCmd.AddCommand(pruneBackupsCmd, pruneWALCmd)
}
