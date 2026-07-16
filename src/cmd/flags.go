package cmd

import (
	"github.com/PrPlanIT/HASteward/src/env"

	"github.com/spf13/pflag"
)

// Flag-adder helpers. Several flags are exposed on more than one command (a command and
// its #31 alias, e.g. `restore` and `backup restore`). These register a flag identically
// on any flagset; env.* dedups the env-var reference by flag name, so each still appears
// once in the docs. Keeping the definition in one place prevents the two copies drifting.

func addMethodFlag(fs *pflag.FlagSet) {
	env.String(fs, &Cfg.BackupMethod, "method", "m", "BACKUP_METHOD", "dump", "Backup method: dump or native")
}

func addSnapshotFlag(fs *pflag.FlagSet) {
	env.String(fs, &Cfg.Snapshot, "snapshot", "", "SNAPSHOT", "latest", "Restic snapshot ID or 'latest' (for restore)")
}

func addHealTimeoutFlag(fs *pflag.FlagSet) {
	env.Int(fs, &Cfg.HealTimeout, "heal-timeout", "", "HEAL_TIMEOUT", 600, "Heal wait timeout in seconds")
}

// addGetFilterFlags registers the snapshot list/filter flags (bound to the shared globals
// the `backup list/policies/repositories` commands read).
func addGetFilterFlags(fs *pflag.FlagSet) {
	fs.BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List across all namespaces")
	fs.StringVarP(&getType, "type", "t", "all", "Snapshot type filter: backup, diverged, or all")
}
