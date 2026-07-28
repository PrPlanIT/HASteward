package restore

import (
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

func regressionTR(authority model.AuthorityOutcome, mostAdvanced string) *model.TriageResult {
	return &model.TriageResult{
		DataComparison: model.DataComparison{Authority: authority, MostAdvanced: mostAdvanced,
			SafeToHeal: authority == model.AuthorityProvable},
		Assessments: []model.InstanceAssessment{{Pod: "pg-1", Timeline: 9, LSN: "2E/BE000070"}},
	}
}

// TestRestoreRegressionDecision is the P3.3 guard: restore mirrors the repair authority
// guard so it cannot discard newer/committed data.
func TestRestoreRegressionDecision(t *testing.T) {
	cfg := func(force bool) *common.Config {
		return &common.Config{ClusterName: "pg", Namespace: "ns", Force: force}
	}

	// leader_not_primary → HARD REFUSE, even with --force (the real data is on a replica).
	for _, force := range []bool{false, true} {
		err := restoreRegressionDecision(regressionTR(model.AuthorityLeaderNotPrimary, "pg-2"), "pg-1", "latest", cfg(force))
		if err == nil || !strings.Contains(err.Error(), "REFUSING") || !strings.Contains(err.Error(), "--force cannot override") {
			t.Fatalf("leader_not_primary must be an un-overridable refusal (force=%v), got: %v", force, err)
		}
	}

	// diverged → refuse without force, proceed (warn) with force.
	if err := restoreRegressionDecision(regressionTR(model.AuthorityDiverged, ""), "pg-1", "latest", cfg(false)); err == nil || !strings.Contains(err.Error(), "DIVERGED") {
		t.Fatalf("diverged restore must refuse without --force, got: %v", err)
	}
	if err := restoreRegressionDecision(regressionTR(model.AuthorityDiverged, ""), "pg-1", "latest", cfg(true)); err != nil {
		t.Fatalf("diverged restore with --force must proceed, got: %v", err)
	}

	// provable primary (safe) → still a rewind of live data: require --force.
	if err := restoreRegressionDecision(regressionTR(model.AuthorityProvable, "pg-1"), "pg-1", "snap-123", cfg(false)); err == nil || !strings.Contains(err.Error(), "REWIND") {
		t.Fatalf("restoring over a healthy primary is a rewind — must require --force, got: %v", err)
	}
	if err := restoreRegressionDecision(regressionTR(model.AuthorityProvable, "pg-1"), "pg-1", "snap-123", cfg(true)); err != nil {
		t.Fatalf("rewind with --force must proceed, got: %v", err)
	}

	// undeterminable → also require --force (can't prove it's safe).
	if err := restoreRegressionDecision(regressionTR(model.AuthorityUndeterminable, ""), "pg-1", "latest", cfg(false)); err == nil {
		t.Fatal("undeterminable authority must require --force before overwriting live data")
	}
}

// TestRestoreRegressionDecision_MessageCarriesPosition: the rewind refusal must name the
// live position at stake so the operator sees exactly what they'd discard.
func TestRestoreRegressionDecision_MessageCarriesPosition(t *testing.T) {
	err := restoreRegressionDecision(regressionTR(model.AuthorityProvable, "pg-1"), "pg-1", "snap-123",
		&common.Config{ClusterName: "pg", Namespace: "ns"})
	if err == nil || !strings.Contains(err.Error(), "timeline 9") || !strings.Contains(err.Error(), "2E/BE000070") {
		t.Fatalf("rewind message must carry the live timeline/LSN at risk, got: %v", err)
	}
}
