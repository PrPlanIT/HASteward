package cmd

import (
	"fmt"
	"os"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/restic"

	"github.com/spf13/cobra"
)

var pruneBackupsCmd = &cobra.Command{
	Use:   "backups",
	Short: "Apply retention policy and remove old backup snapshots",
	Long: `Prunes old backup snapshots from restic repositories according to the
configured retention policy (keep-last, keep-daily, keep-weekly, keep-monthly).

By default, only type=backup snapshots are pruned. Use -t diverged to prune
only diverged snapshots, or -t all to prune both types.

For diverged snapshots, retention is group-aware: snapshots sharing the same
job tag (from one repair operation) are kept or removed as a unit. So
--keep-last 3 means "keep the 3 most recent repair jobs" regardless of how
many instances each job captured.

Examples:
  hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups
  hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups \
    --keep-last 7 --keep-daily 30 --keep-weekly 12 --keep-monthly 24
  hasteward prune backups -e cnpg -c zitadel-postgres -n zeldas-lullaby --backups-path /backups \
    -t diverged --keep-last 3`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if Cfg.BackupsPath == "" {
			return fmt.Errorf("prune backups requires --backups-path")
		}
		if Cfg.ResticPassword == "" {
			return fmt.Errorf("prune backups requires RESTIC_PASSWORD env var")
		}
		if Cfg.Engine == "" {
			return fmt.Errorf("prune backups requires --engine/-e")
		}
		if Cfg.ClusterName == "" {
			return fmt.Errorf("prune backups requires --cluster/-c")
		}
		if Cfg.Namespace == "" {
			return fmt.Errorf("prune backups requires --namespace/-n")
		}

		switch pbType {
		case "backup", "diverged", "all":
		default:
			return fmt.Errorf("--type must be backup, diverged, or all (got %q)", pbType)
		}

		if Cfg.Verbose {
			os.Setenv(common.EnvPrefix+"LOG_LEVEL", "debug")
			common.InitLogging(false)
		}
		if Cfg.ResticPassword != "" {
			common.RegisterSecret(Cfg.ResticPassword)
		}

		rc := restic.NewClient(Cfg.BackupsPath, Cfg.ResticPassword)

		baseTags := map[string]string{
			"engine":    Cfg.Engine,
			"cluster":   Cfg.ClusterName,
			"namespace": Cfg.Namespace,
		}

		policy := restic.RetentionPolicy{
			KeepLast:    pbKeepLast,
			KeepDaily:   pbKeepDaily,
			KeepWeekly:  pbKeepWeekly,
			KeepMonthly: pbKeepMonthly,
		}

		common.InfoLog("Applying retention policy (type=%s): keep-last=%d keep-daily=%d keep-weekly=%d keep-monthly=%d",
			pbType, policy.KeepLast, policy.KeepDaily, policy.KeepWeekly, policy.KeepMonthly)

		totalKeep := 0
		totalRemove := 0

		if pbType == "backup" || pbType == "all" {
			tags := copyTags(baseTags)
			tags["type"] = "backup"
			results, err := rc.Forget(cmd.Context(), tags, policy, pbType == "backup")
			if err != nil {
				return fmt.Errorf("prune (backup) failed: %w", err)
			}
			for _, r := range results {
				totalKeep += len(r.Keep)
				totalRemove += len(r.Remove)
			}
		}

		if pbType == "diverged" || pbType == "all" {
			tags := copyTags(baseTags)
			tags["type"] = "diverged"
			kept, removed, err := rc.ForgetGrouped(cmd.Context(), tags, policy, true)
			if err != nil {
				return fmt.Errorf("prune (diverged) failed: %w", err)
			}
			totalKeep += kept
			totalRemove += removed
		}

		fmt.Printf("Pruned %d snapshots, kept %d\n", totalRemove, totalKeep)
		return nil
	},
}

var (
	pbKeepLast    int
	pbKeepDaily   int
	pbKeepWeekly  int
	pbKeepMonthly int
	pbType        string
)

func copyTags(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func init() {
	pruneBackupsCmd.Flags().IntVar(&pbKeepLast, "keep-last", 7, "Keep the last N snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepDaily, "keep-daily", 30, "Keep N daily snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepWeekly, "keep-weekly", 12, "Keep N weekly snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().IntVar(&pbKeepMonthly, "keep-monthly", 24, "Keep N monthly snapshots (or jobs for diverged)")
	pruneBackupsCmd.Flags().StringVarP(&pbType, "type", "t", "backup", "Snapshot type to prune: backup, diverged, or all")
}
