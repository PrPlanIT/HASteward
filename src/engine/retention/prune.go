package retention

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/restic"
)

// runPrune applies the retention policy to one engine's snapshot sets — "backup"
// (Forget) and/or "diverged" (ForgetGrouped) per opts.Type — and tallies what was
// kept vs removed. Identical across engines; only the "engine" tag value differs.
func runPrune(ctx context.Context, cfg *common.Config, engine string, opts PruneOptions) (*model.PruneResult, error) {
	rc := restic.NewClient(cfg.BackupsPath, cfg.ResticPassword)

	baseTags := map[string]string{
		"engine":    engine,
		"cluster":   cfg.ClusterName,
		"namespace": cfg.Namespace,
	}

	policy := restic.RetentionPolicy{
		KeepLast:    opts.KeepLast,
		KeepDaily:   opts.KeepDaily,
		KeepWeekly:  opts.KeepWeekly,
		KeepMonthly: opts.KeepMonthly,
	}

	common.InfoLog("Applying retention policy (type=%s): keep-last=%d keep-daily=%d keep-weekly=%d keep-monthly=%d",
		opts.Type, policy.KeepLast, policy.KeepDaily, policy.KeepWeekly, policy.KeepMonthly)

	totalKeep := 0
	totalRemove := 0

	if opts.Type == "backup" || opts.Type == "all" {
		tags := copyTags(baseTags)
		tags["type"] = "backup"
		results, err := rc.Forget(ctx, tags, policy, opts.Type == "backup")
		if err != nil {
			return nil, fmt.Errorf("prune (backup) failed: %w", err)
		}
		for _, r := range results {
			totalKeep += len(r.Keep)
			totalRemove += len(r.Remove)
		}
	}

	if opts.Type == "diverged" || opts.Type == "all" {
		tags := copyTags(baseTags)
		tags["type"] = "diverged"
		kept, removed, err := rc.ForgetGrouped(ctx, tags, policy, true)
		if err != nil {
			return nil, fmt.Errorf("prune (diverged) failed: %w", err)
		}
		totalKeep += kept
		totalRemove += removed
	}

	return &model.PruneResult{
		TotalKept:    totalKeep,
		TotalRemoved: totalRemove,
	}, nil
}

func copyTags(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
