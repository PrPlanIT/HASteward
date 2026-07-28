package retention

import (
	"context"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
)

// TestRunPrune_DryRunDeletesNothing guards the audit fix: prune-backups (backup prune)
// must honor --dry-run and forget NOTHING. If the dry-run guard failed to short-circuit,
// rc.Forget would run against the (nonexistent) repo and error — so a nil error with a
// zero result is proof the guard stopped before any restic mutation.
func TestRunPrune_DryRunDeletesNothing(t *testing.T) {
	cfg := &common.Config{
		DryRun:         true,
		BackupsPath:    "/nonexistent/repo", // touching it would error — proving we did NOT
		ResticPassword: "x",
		ClusterName:    "c",
		Namespace:      "n",
	}
	res, err := runPrune(context.Background(), cfg, "cnpg", PruneOptions{Type: "all", KeepLast: 7})
	if err != nil {
		t.Fatalf("dry-run must not touch the repo (no error), got: %v", err)
	}
	if res.TotalRemoved != 0 || res.TotalKept != 0 {
		t.Fatalf("dry-run must forget nothing, got removed=%d kept=%d", res.TotalRemoved, res.TotalKept)
	}
}
