package repair

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

func TestBuildUntargetedPlan(t *testing.T) {
	t.Run("split-brain -> hard stop (no --force override)", func(t *testing.T) {
		if _, err := buildUntargetedPlan(triageRes(false), "nodes"); err == nil {
			t.Fatal("split-brain must hard-stop untargeted repair")
		}
	})

	t.Run("nothing needs heal -> (nil, nil)", func(t *testing.T) {
		targets, err := buildUntargetedPlan(
			triageRes(true, model.InstanceAssessment{Pod: "c-0", NeedsHeal: false}), "nodes")
		if err != nil || targets != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", targets, err)
		}
	})

	t.Run("heals only the flagged members, reason from notes", func(t *testing.T) {
		targets, err := buildUntargetedPlan(triageRes(true,
			model.InstanceAssessment{Pod: "c-0", Instance: 0, NeedsHeal: false},
			model.InstanceAssessment{Pod: "c-1", Instance: 1, NeedsHeal: true, Notes: []string{"crashloop"}},
		), "nodes")
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 || targets[0].Pod != "c-1" || targets[0].Reason != "crashloop" {
			t.Fatalf("want [c-1: crashloop], got %v", targets)
		}
	})
}
