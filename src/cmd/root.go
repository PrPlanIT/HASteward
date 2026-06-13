package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/printer"
	"github.com/PrPlanIT/HASteward/src/output/style"

	"github.com/spf13/cobra"
)

// Cfg is the shared runtime configuration bound to root persistent flags.
var Cfg common.Config

// outputMode holds the raw --output flag value before parsing.
var outputMode string

// dryRun holds the --dry-run flag state.
var dryRun bool

// P is the active printer for the current command invocation.
var P *printer.Printer

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
	pf.BoolVarP(&Cfg.Force, "force", "f", common.EnvBool("FORCE", false),
		"Override automatic safety refusal for targeted repair. In ambiguous Galera\n"+
			"recovery states (divergent UUIDs, split-brain, no clear primary), --donor is\n"+
			"required to declare the authoritative source node.")
	pf.StringVar(&Cfg.BackupsPath, "backups-path", common.Env("BACKUPS_PATH", ""), "Restic repository path or URL")
	pf.StringVar(&Cfg.ResticPassword, "restic-password", common.EnvRaw("RESTIC_PASSWORD", common.Env("RESTIC_PASSWORD", "")), "Restic repository encryption password")
	pf.BoolVar(&Cfg.NoEscrow, "no-escrow", common.EnvBool("NO_ESCROW", false), "Skip pre-repair escrow backup")
	pf.BoolVar(&Cfg.Unwedge, "unwedge", common.EnvBool("UNWEDGE", false), "CNPG deadlock breaker: clear a disposable replica's datadir offline (escrow-gated) to un-freeze a disk-full cluster. Use --dry-run first.")
	pf.BoolVar(&Cfg.WipeDatadir, "wipe-datadir", common.EnvBool("WIPE_DATADIR", false),
		"Wipe entire datadir on target instance (not just grastate). Forces full SST\n"+
			"reseed from donor. Use when local data is irrecoverably corrupted. Requires\n"+
			"--force and --instance.")
	pf.BoolVar(&Cfg.FixBootstrap, "fix-bootstrap", common.EnvBool("FIX_BOOTSTRAP", false),
		"Reconfigure: clear grastate and remove bootstrap config on target instance.\n"+
			"Prevents stale local bootstrap behavior during cluster restart.")
	pf.StringVarP(&Cfg.BackupMethod, "method", "m", common.Env("BACKUP_METHOD", "dump"), "Backup method: dump or native")
	pf.StringVar(&Cfg.Snapshot, "snapshot", common.Env("SNAPSHOT", "latest"), "Restic snapshot ID or 'latest' (for restore)")
	pf.IntVar(&Cfg.HealTimeout, "heal-timeout", common.EnvInt("HEAL_TIMEOUT", 600), "Heal wait timeout in seconds")
	pf.IntVar(&Cfg.DeleteTimeout, "delete-timeout", common.EnvInt("DELETE_TIMEOUT", 300), "Delete wait timeout in seconds")
	pf.StringVar(&Cfg.Kubeconfig, "kubeconfig", common.EnvRaw("KUBECONFIG", ""), "Path to kubeconfig file")
	pf.BoolVarP(&Cfg.Verbose, "verbose", "v", common.EnvBool("VERBOSE", false), "Verbose output (debug logging)")
	pf.BoolVar(&dryRun, "dry-run", false, "Show planned actions without executing (destructive commands)")
	pf.StringVar(&outputMode, "output", common.Env("OUTPUT", "auto"), "Output format: auto, human, json, jsonl")
	pf.Bool("no-color", false, "Disable color output")
	pf.Bool("debug", false, "Enable debug output")

	// Instance and donor flags need special handling for optional int
	pf.StringP("instance", "i", common.Env("INSTANCE", ""), "Target specific instance number")
	pf.StringP("donor", "d", common.Env("DONOR", ""), "Explicit donor instance ordinal (declares authoritative source for repair)")

	RootCmd.AddCommand(triageCmd, repairCmd, reconfigureCmd, backupCmd, restoreCmd, serveCmd, getCmd, exportCmd, pruneCmd)
}

// IsDryRun returns whether --dry-run was specified.
func IsDryRun() bool {
	return dryRun
}

// InitPrinter creates the printer for the current command.
// In json/jsonl modes, legacy output functions are silenced so only the
// printer writes to stdout.
func InitPrinter(command string) (*printer.Printer, error) {
	mode, err := printer.ParseOutputMode(outputMode)
	if err != nil {
		return nil, err
	}
	P = printer.New(mode, command)
	// Silence legacy output.* functions in machine-output modes
	output.SetEnabled(P.IsHuman())
	return P, nil
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

// ResolveDonor parses the --donor flag into Cfg.DonorInstance.
func ResolveDonor(cmd *cobra.Command) error {
	raw, _ := cmd.Flags().GetString("donor")
	if raw == "" {
		Cfg.DonorInstance = nil
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("--donor must be an integer, got %q", raw)
	}
	if n < 0 {
		return fmt.Errorf("--donor must be non-negative, got %d", n)
	}
	Cfg.DonorInstance = &n
	return nil
}

// PreRun validates required flags, initializes K8s clients, and resolves the engine provider.
func PreRun(cmd *cobra.Command, mode string) (provider.EngineProvider, error) {
	Cfg.Mode = mode

	debug, _ := cmd.Flags().GetBool("debug")
	if Cfg.Verbose || debug {
		os.Setenv(common.EnvPrefix+"LOG_LEVEL", "debug")
		common.InitLogging(false)
	}

	noColor, _ := cmd.Flags().GetBool("no-color")
	if noColor {
		style.SetColorEnabled(false)
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
	if err := ResolveDonor(cmd); err != nil {
		return nil, err
	}

	// --wipe-datadir requires --force and --instance
	if Cfg.WipeDatadir {
		if !Cfg.Force {
			return nil, fmt.Errorf("--wipe-datadir requires --force (this is a destructive operation)")
		}
		if Cfg.InstanceNumber == nil {
			return nil, fmt.Errorf("--wipe-datadir requires --instance (must target a specific node)")
		}
	}

	if _, err := k8s.Init(Cfg.Kubeconfig); err != nil {
		return nil, fmt.Errorf("kubernetes init failed: %w", err)
	}

	prov, err := provider.GetProvider(Cfg.Engine)
	if err != nil {
		return nil, err
	}

	ctx := cmd.Context()

	// In human mode, print the legacy header
	if P == nil || P.IsHuman() {
		output.Header(prov.Name(), mode, Cfg.ClusterName, Cfg.Namespace)
	}

	if err := prov.Validate(ctx, &Cfg); err != nil {
		return nil, err
	}

	return prov, nil
}
