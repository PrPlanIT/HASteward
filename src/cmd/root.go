package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	"github.com/spf13/cobra"
)

// Cfg is the shared runtime configuration bound to root persistent flags.
var Cfg common.Config

// RootCmd is the top-level cobra command.
var RootCmd = &cobra.Command{
	Use:   "hasteward",
	Short: "HASteward - High Availability Steward for database clusters",
	Long: `HASteward safely triages, repairs, backs up, and restores
database clusters managed by CNPG (PostgreSQL) and MariaDB Operator (Galera).

Backups are stored in restic repositories with block-level dedup,
encryption, and compression.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	pf := RootCmd.PersistentFlags()
	pf.StringVarP(&Cfg.Engine, "engine", "e", common.Env("ENGINE", ""), "Database engine: cnpg or galera")
	pf.StringVarP(&Cfg.ClusterName, "cluster", "c", common.Env("CLUSTER", ""), "Database cluster CR name")
	pf.StringVarP(&Cfg.Namespace, "namespace", "n", common.Env("NAMESPACE", ""), "Kubernetes namespace")
	pf.BoolVarP(&Cfg.Force, "force", "f", common.EnvBool("FORCE", false), "Override safety checks (targeted repair only)")
	pf.StringVar(&Cfg.BackupsPath, "backups-path", common.Env("BACKUPS_PATH", ""), "Restic repository path or URL")
	pf.StringVar(&Cfg.ResticPassword, "restic-password", common.EnvRaw("RESTIC_PASSWORD", common.Env("RESTIC_PASSWORD", "")), "Restic repository encryption password")
	pf.BoolVar(&Cfg.NoEscrow, "no-escrow", common.EnvBool("NO_ESCROW", false), "Skip pre-repair escrow backup")
	pf.StringVarP(&Cfg.BackupMethod, "method", "m", common.Env("BACKUP_METHOD", "dump"), "Backup method: dump or native")
	pf.StringVar(&Cfg.Snapshot, "snapshot", common.Env("SNAPSHOT", "latest"), "Restic snapshot ID or 'latest' (for restore)")
	pf.IntVar(&Cfg.HealTimeout, "heal-timeout", common.EnvInt("HEAL_TIMEOUT", 600), "Heal wait timeout in seconds")
	pf.IntVar(&Cfg.DeleteTimeout, "delete-timeout", common.EnvInt("DELETE_TIMEOUT", 300), "Delete wait timeout in seconds")
	pf.StringVar(&Cfg.Kubeconfig, "kubeconfig", common.EnvRaw("KUBECONFIG", ""), "Path to kubeconfig file")
	pf.BoolVarP(&Cfg.Verbose, "verbose", "v", common.EnvBool("VERBOSE", false), "Verbose output (debug logging)")

	// Instance flag needs special handling for optional int
	pf.StringP("instance", "i", common.Env("INSTANCE", ""), "Target specific instance number")

	RootCmd.AddCommand(triageCmd, repairCmd, backupCmd, restoreCmd, serveCmd, getCmd, exportCmd, pruneCmd)
}

// ResolveInstance parses the --instance flag into Cfg.InstanceNumber.
func ResolveInstance(cmd *cobra.Command) error {
	raw, _ := cmd.Flags().GetString("instance")
	if raw == "" {
		Cfg.InstanceNumber = nil
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("--instance must be an integer, got %q", raw)
	}
	Cfg.InstanceNumber = &n
	return nil
}

// PreRun validates required flags, initializes K8s clients, and resolves the engine.
func PreRun(cmd *cobra.Command, mode string) (engine.Engine, error) {
	Cfg.Mode = mode

	if Cfg.Verbose {
		os.Setenv(common.EnvPrefix+"LOG_LEVEL", "debug")
		common.InitLogging(false)
	}

	if Cfg.ResticPassword != "" {
		common.RegisterSecret(Cfg.ResticPassword)
	}

	var missing []string
	if Cfg.Engine == "" {
		missing = append(missing, "--engine/-e")
	}
	if Cfg.ClusterName == "" {
		missing = append(missing, "--cluster/-c")
	}
	if Cfg.Namespace == "" {
		missing = append(missing, "--namespace/-n")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required flags: %s", strings.Join(missing, ", "))
	}

	if err := ResolveInstance(cmd); err != nil {
		return nil, err
	}

	if _, err := k8s.Init(Cfg.Kubeconfig); err != nil {
		return nil, fmt.Errorf("kubernetes init failed: %w", err)
	}

	eng, err := engine.Get(Cfg.Engine)
	if err != nil {
		return nil, err
	}

	ctx := cmd.Context()
	output.Header(eng.Name(), mode, Cfg.ClusterName, Cfg.Namespace)
	if err := eng.Validate(ctx, &Cfg); err != nil {
		return nil, err
	}

	return eng, nil
}
