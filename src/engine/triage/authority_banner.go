package triage

import (
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// renderAuthorityBanner prints the operator-facing verdict for a cross-instance
// data comparison, with guidance specific to the authority outcome. It is shared by
// every engine's Analyze (CNPG, Galera, future) so they speak one language and a new
// failure mode is explained the same way everywhere. No-op when the heal is safe.
func renderAuthorityBanner(cmp model.DataComparison) {
	if cmp.SafeToHeal {
		return
	}
	const rule = "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
	output.Println()
	output.Println(rule)
	switch cmp.Authority {
	case model.AuthorityUndeterminable:
		output.Println("  REFUSING TO HEAL — AUTHORITY UNDETERMINABLE")
		output.Println("  A node that could hold data could not be read; deciding past it")
		output.Println("  risks discarding the newest data. Bring it up for inspection")
		output.Println("  (fenced read) and re-triage — do NOT heal while blind.")
	case model.AuthorityDiverged:
		output.Println("  REFUSING TO HEAL — TRUE DIVERGENCE (SPLIT-BRAIN)")
		output.Println("  Committed data exists on more than one lineage past a shared")
		output.Println("  fork. No node is a safe authority — escrow ALL and choose the")
		output.Println("  surviving lineage manually.")
	case model.AuthorityLeaderNotPrimary:
		output.Println("  REFUSING TO HEAL — AUTHORITY IS NOT THE CURRENT PRIMARY")
		output.Printf("  The winning data is on %s, not the primary. Promote it first;\n", cmp.MostAdvanced)
		output.Println("  healing from the primary would discard the newer data.")
	default:
		output.Println("  CRITICAL: NOT SAFE TO HEAL — authority is undeterminable")
		output.Println("  Triage could not prove a single, consistent most-advanced history.")
	}
	if len(cmp.SplitBrainDetails) > 0 {
		output.Println("  Reason(s):")
		for _, sb := range cmp.SplitBrainDetails {
			output.Printf("    - %s\n", sb)
		}
	}
	output.Println("  DO NOT blindly heal — review the data above and decide manually.")
	output.Println(rule)
	output.Println()
}
