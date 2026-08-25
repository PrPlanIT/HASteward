package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/env"
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
	// Each flag is declared once via env.*, which creates the flag, seeds its default
	// from the environment, and registers the flag↔variable link for the generated
	// Environment Variables reference. Type/default/description stay owned by the flag.
	env.String(pf, &Cfg.Engine, "engine", "e", "ENGINE", "", "Database engine: cnpg or galera")
	env.String(pf, &Cfg.ClusterName, "cluster", "c", "CLUSTER", "", "Database cluster CR name")
	env.String(pf, &Cfg.Namespace, "namespace", "n", "NAMESPACE", "", "Kubernetes namespace")
	env.Bool(pf, &Cfg.Force, "force", "f", "FORCE", false,
		"Override automatic safety refusal for targeted repair. In ambiguous Galera\n"+
			"recovery states (divergent UUIDs, split-brain, no clear primary), --donor is\n"+
			"required to declare the authoritative source node.")
	env.String(pf, &Cfg.BackupsPath, "backups-path", "", "BACKUPS_PATH", "", "Restic repository path or URL")
	env.RawOrPrefixed(pf, &Cfg.ResticPassword, "restic-password", "", "RESTIC_PASSWORD", "", "Restic repository encryption password")
	env.Bool(pf, &Cfg.NoEscrow, "no-escrow", "", "NO_ESCROW", false, "Skip pre-repair escrow backup")
	// Command-specific flags (demoted from persistent — #31: flag scope = "which cluster"
	// stays global, "how this algorithm behaves" belongs on the owning command).
	env.Bool(repairCmd.Flags(), &Cfg.Unwedge, "unwedge", "", "UNWEDGE", false, "CNPG deadlock breaker: clear a disposable replica's datadir offline (escrow-gated) to un-freeze a disk-full cluster. Use --dry-run first.")
	env.Bool(repairCmd.Flags(), &Cfg.Promote, "promote", "", "PROMOTE", false, "CNPG rebuild-around-authority: escrow the cluster + persist a proof + print the swap runbook to promote --instance N when the authority is not the primary. Use --dry-run first.")
	env.Bool(pruneWALTopCmd.Flags(), &Cfg.DeadlockRecover, "deadlock-recover", "", "DEADLOCK_RECOVER", false, "CNPG disk-full DEADLOCK recovery: replay + recycle WAL IN PLACE for an instance too full to start (escrow-snapshot → single-user replay → archivecleanup). For when plain prune-wal finds nothing to trim. Use --dry-run first.")
	env.String(pruneWALTopCmd.Flags(), &Cfg.SnapshotClass, "snapshot-class", "", "SNAPSHOT_CLASS", "", "VolumeSnapshotClass for the deadlock-recover escrow (auto-discovered from the PVC provisioner if empty)")
	env.Bool(repairCmd.Flags(), &Cfg.WipeDatadir, "wipe-datadir", "", "WIPE_DATADIR", false,
		"Wipe entire datadir on target instance (not just grastate). Forces full SST\n"+
			"reseed from donor. Use when local data is irrecoverably corrupted. Requires\n"+
			"--force and --instance.")
	env.Bool(reconfigureCmd.Flags(), &Cfg.FixBootstrap, "fix-bootstrap", "", "FIX_BOOTSTRAP", false,
		"reset-authority: clear grastate and remove bootstrap config on target instance.\n"+
			"Prevents stale local bootstrap behavior during cluster restart.")
	// --method (backup+restore), --snapshot (restore+export), --heal-timeout (repair+
	// reconfigure): behavior knobs, demoted to their consumers. env dedups the binding so
	// each still appears once in the env reference. --delete-timeout stays global (used by
	// triage/bootstrap/repair/prune-wal/reconfigure — effectively a global default), as
	// does --instance (addressing: "which node").
	addMethodFlag(restoreCmd.Flags())
	addSnapshotFlag(restoreCmd.Flags())
	addSnapshotFlag(exportCmd.Flags())
	addHealTimeoutFlag(repairCmd.Flags())
	addHealTimeoutFlag(reconfigureCmd.Flags())
	env.Int(pf, &Cfg.DeleteTimeout, "delete-timeout", "", "DELETE_TIMEOUT", 300, "Delete wait timeout in seconds")
	env.Int(pf, &Cfg.ExpandTargetPct, "expand-target-pct", "", "EXPAND_TARGET_PCT", 60,
		"When triage recommends expanding a genuinely data-full PVC, size the suggestion so\n"+
			"post-expansion data lands at ~this percent of the volume (leaving headroom for\n"+
			"WAL/temp/growth). Set HASTEWARD_EXPAND_TARGET_PCT once to avoid repeating the flag.")
	env.Raw(pf, &Cfg.Kubeconfig, "kubeconfig", "", "KUBECONFIG", "", "Path to kubeconfig file")
	env.Bool(pf, &Cfg.Verbose, "verbose", "v", "VERBOSE", false, "Verbose output (debug logging)")
	pf.BoolVar(&dryRun, "dry-run", false, "Show planned actions without executing (destructive commands)")
	env.String(pf, &outputMode, "output", "", "OUTPUT", "auto", "Output format: auto, human, json, jsonl")
	pf.Bool("no-color", false, "Disable color output")
	pf.Bool("debug", false, "Enable debug output")

	// Instance and donor flags need special handling for optional int. --donor is
	// repair-only; --instance is demoted to its consumers in each command's file.
	env.StringP(pf, "instance", "i", "INSTANCE", "", "Target specific instance number")
	env.StringP(repairCmd.Flags(), "donor", "d", "DONOR", "", "Explicit donor instance ordinal (declares authoritative source for repair)")

	// Top-level: diagnose (triage/status via their own files), recover (repair/bootstrap/
	// reconfigure/prune-wal), protect (backup group), operate (serve/docs/version). restore/
	// export/get/prune are reparented under `backup` — not top-level (#31 Phase 2).
	RootCmd.AddCommand(triageCmd, repairCmd, reconfigureCmd, backupCmd, serveCmd)
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

	// Populate --dry-run ONCE, here, for every command that goes through PreRun — so no
	// individual command can forget to wire it (the trap that let prune-wal ignore
	// --dry-run and silently mutate). Each mutating engine is responsible for honoring
	// Cfg.DryRun by stopping before its first mutation.
	Cfg.DryRun = IsDryRun()

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
