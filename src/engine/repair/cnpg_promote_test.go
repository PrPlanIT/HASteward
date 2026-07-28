package repair

import (
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

func promoteTR(authority model.AuthorityOutcome, mostAdvanced string, pods ...string) *model.TriageResult {
	tr := &model.TriageResult{
		Cluster:        model.ObjectRef{Name: "pg", Namespace: "ns"},
		DataComparison: model.DataComparison{Authority: authority, MostAdvanced: mostAdvanced, SafeToHeal: authority == model.AuthorityProvable},
	}
	for _, p := range pods {
		tr.Assessments = append(tr.Assessments, model.InstanceAssessment{Pod: p})
	}
	return tr
}

// TestPlanPromotion_LeaderNotPrimary: promoting the proven authority is valid and plans
// to rebuild the others from it; promoting any OTHER instance is refused.
func TestPlanPromotion_LeaderNotPrimary(t *testing.T) {
	tr := promoteTR(model.AuthorityLeaderNotPrimary, "pg-2", "pg-1", "pg-2", "pg-3")

	plan, err := planPromotion(tr, 2, false)
	if err != nil {
		t.Fatalf("promoting the proven authority pg-2 must be valid, got: %v", err)
	}
	if plan.Authority != "pg-2" || plan.Diverged {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if strings.Join(plan.Rebuild, ",") != "pg-1,pg-3" {
		t.Fatalf("rebuild set must be the other instances, got %v", plan.Rebuild)
	}
	if len(plan.RecoverySet) != 3 || plan.RecoverySet[0] != "pg-2" {
		t.Fatalf("recovery set must be all instances with the authority first, got %v", plan.RecoverySet)
	}

	// Promoting a non-authority under leader_not_primary → refused, names the real authority.
	if _, err := planPromotion(tr, 3, false); err == nil || !strings.Contains(err.Error(), "authority is pg-2") {
		t.Fatalf("promoting pg-3 must be refused (authority is pg-2), got: %v", err)
	}
	// Even --force cannot make pg-3 the authority under leader_not_primary.
	if _, err := planPromotion(tr, 3, true); err == nil {
		t.Fatal("promoting a non-authority must be refused even with --force")
	}
}

// TestPlanPromotion_Diverged: no proven authority, so a designation needs --force.
func TestPlanPromotion_Diverged(t *testing.T) {
	tr := promoteTR(model.AuthorityDiverged, "", "pg-1", "pg-2", "pg-3")

	if _, err := planPromotion(tr, 2, false); err == nil || !strings.Contains(err.Error(), "DIVERGED") {
		t.Fatalf("diverged promotion must require --force, got: %v", err)
	}
	plan, err := planPromotion(tr, 2, true)
	if err != nil {
		t.Fatalf("diverged promotion with --force must be allowed, got: %v", err)
	}
	if !plan.Diverged || plan.Authority != "pg-2" {
		t.Fatalf("expected a diverged plan promoting pg-2, got %+v", plan)
	}
}

// TestPlanPromotion_ProvableAndUndeterminable: nothing to promote when the primary is the
// authority; promoting a behind replica or under undeterminable is refused.
func TestPlanPromotion_ProvableAndUndeterminable(t *testing.T) {
	prov := promoteTR(model.AuthorityProvable, "pg-1", "pg-1", "pg-2", "pg-3")

	// Target IS the authority/primary → nothing to promote.
	if _, err := planPromotion(prov, 1, false); err == nil || !strings.Contains(err.Error(), "already the authority") {
		t.Fatalf("promoting the current authority must be a no-op refusal, got: %v", err)
	}
	// Target is a behind replica while the primary is authoritative → data-loss refusal, even forced.
	if _, err := planPromotion(prov, 2, true); err == nil || !strings.Contains(err.Error(), "DISCARD") {
		t.Fatalf("promoting a behind replica over a provable primary must be refused, got: %v", err)
	}

	undet := promoteTR(model.AuthorityUndeterminable, "", "pg-1", "pg-2")
	if _, err := planPromotion(undet, 2, true); err == nil || !strings.Contains(err.Error(), "UNDETERMINABLE") {
		t.Fatalf("promotion under undeterminable authority must be refused until inspection, got: %v", err)
	}
}

// TestPlanPromotion_UnknownInstance: an out-of-range instance is refused before anything.
func TestPlanPromotion_UnknownInstance(t *testing.T) {
	tr := promoteTR(model.AuthorityLeaderNotPrimary, "pg-2", "pg-1", "pg-2")
	if _, err := planPromotion(tr, 9, true); err == nil || !strings.Contains(err.Error(), "not among the cluster's instances") {
		t.Fatalf("unknown instance must be refused, got: %v", err)
	}
}

// TestCnpgPromotionRunbook: the runbook names the authority, the rebuild set, and is
// explicit that the in-place swap is the manual/not-yet-automated step.
func TestCnpgPromotionRunbook(t *testing.T) {
	plan := newPromotionPlan("pg-2", []string{"pg-1", "pg-3"}, false)
	rb := cnpgPromotionRunbook("pg", "ns", plan)
	for _, want := range []string{"pg-2", "--instance 2", "not yet automated", "hasteward repair -e cnpg -c pg -n ns", "rollback"} {
		if !strings.Contains(rb, want) {
			t.Fatalf("runbook missing %q:\n%s", want, rb)
		}
	}
}
