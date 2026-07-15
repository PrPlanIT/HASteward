package repair

import (
	"fmt"
	"strings"

	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// buildUntargetedPlan is the shared auto (all-members) heal plan: refuse on a
// split-brain (no --force override for untargeted), then heal every member
// flagged NeedsHeal. noun is the engine's word for its members ("nodes" /
// "replicas"), used only in operator-facing messages.
//
// (planTargeted is intentionally NOT shared: the engines' primary protection
// genuinely diverges — galera warns on IsInPrimary, cnpg hard-aborts on the
// current primary — and unifying that load-bearing safety gate is not worth the
// risk for the small remaining overlap.)
func buildUntargetedPlan(result *model.TriageResult, noun string) ([]HealTarget, error) {
	if !result.DataComparison.SafeToHeal {
		return nil, fmt.Errorf("HARD STOP: Split-brain detected. Cannot auto-heal all %s. "+
			"Admin must review triage output, then use targeted repair: --instance <N>", noun)
	}

	var targets []HealTarget
	for _, a := range result.Assessments {
		if a.NeedsHeal {
			reason := "needs heal"
			if len(a.Notes) > 0 {
				reason = strings.Join(a.Notes, ", ")
			}
			targets = append(targets, HealTarget{Pod: a.Pod, InstanceNum: a.Instance, Reason: reason})
		}
	}

	if len(targets) == 0 {
		output.Info("All %s are healthy. Nothing to heal.", noun)
		return nil, nil
	}

	output.Section("Repair Plan")
	for _, t := range targets {
		output.Bullet(0, "%s (%s)", t.Pod, t.Reason)
	}
	return targets, nil
}
