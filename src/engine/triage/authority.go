package triage

import "github.com/PrPlanIT/HASteward/src/output/model"

// This file holds the authority-verdict assembly shared by every engine's
// crossInstanceComparison + Analyze (CNPG, Galera, future). Each engine computes its
// own model-specific verdict — CNPG by WAL lineage, Galera by seqno-within-UUID — but
// they render it into the SAME model.DataComparison shape, AuthorityOutcome, warnings,
// and AuthorityStatus. Those common steps live here so a new engine inherits them and
// the two cannot drift.
//
// NOTE ON SCOPE: the per-node legibility MODELS are deliberately NOT unified. CNPG's
// three-valued ReadState (Read/Unread/AbsentNoData) and Galera's two-valued
// effectiveSeqno.Known + unread slice reflect genuinely different authority models;
// forcing one type onto both would rewrite well-tested, subtle ranking code for little
// gain. What IS shared — and what actually duplicated — is the OUTPUT assembly below.

// newAuthorityComparison assembles the common DataComparison fields from an authority
// verdict: SafeToHeal is exactly (outcome == AuthorityProvable); reasons become the
// SplitBrainDetails and drive the warnings. Engine-specific fields (CNPG's
// CheckpointLocation, Galera's PrimaryMembers/BestPrimarySeqno) are set by the caller
// on the returned value.
func newAuthorityComparison(outcome model.AuthorityOutcome, mostAdvanced string, mostAdvancedValue int64, reasons []string, okMsg string) model.DataComparison {
	safe := outcome == model.AuthorityProvable
	return model.DataComparison{
		MostAdvanced:      mostAdvanced,
		MostAdvancedValue: mostAdvancedValue,
		SafeToHeal:        safe,
		Authority:         outcome,
		Warnings:          authorityWarnings(safe, okMsg, reasons),
		SplitBrainDetails: reasons,
	}
}

// authorityWarnings renders the operator warning lines: the single OK message when
// safe, else each refusal reason prefixed "SPLIT-BRAIN RISK:". Identical across engines.
func authorityWarnings(safe bool, okMsg string, reasons []string) []string {
	if safe {
		if okMsg == "" {
			return nil
		}
		return []string{okMsg}
	}
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, "SPLIT-BRAIN RISK: "+r)
	}
	return out
}

// deriveAuthorityStatus projects SafeToHeal into the TriageResult.AuthorityStatus
// string the repair PreAssess gate reads. Donor recommendation stays engine-specific.
func deriveAuthorityStatus(safeToHeal bool) string {
	if safeToHeal {
		return "unambiguous"
	}
	return "ambiguous"
}
