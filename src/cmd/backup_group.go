package cmd

import "github.com/spf13/cobra"

// #31 Phase 2: everything backup-related lives under the `backup` noun. The real commands
// are REPARENTED here (no aliases, no delegation): the old top-level `restore`/`export`,
// the `get` group, and `prune backups` are gone. `create` is the explicit verb — bare
// `backup` is group-only.
var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a backup of a database cluster",
}

func init() {
	// The create action becomes the explicit `backup create`; bare `backup` is a group.
	backupCreateCmd.RunE = backupCmd.RunE
	backupCmd.RunE = nil
	addMethodFlag(backupCreateCmd.Flags())

	// The list-type commands lose getCmd's inherited --type/--all-namespaces on reparent;
	// give them their own (bound to the same globals their RunE reads).
	addGetFilterFlags(getBackupsCmd.Flags())
	addGetFilterFlags(getPoliciesCmd.Flags())
	addGetFilterFlags(getRepositoriesCmd.Flags())

	backupCmd.AddCommand(
		backupCreateCmd,    // backup create
		getBackupsCmd,      // backup list
		restoreCmd,         // backup restore
		exportCmd,          // backup export
		pruneBackupsCmd,    // backup prune
		getPoliciesCmd,     // backup policies
		getRepositoriesCmd, // backup repositories
	)
}
