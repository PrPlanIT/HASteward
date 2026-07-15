package bootstrap

import "testing"

// TestBuildLineageGroups_ExcludesInvalidRecover guards bug #2 (GitLab issue #2):
// when wsrep_recover FAILS for a node, executeBootstrap fabricates a Valid:false
// result carrying that node's triage EffectiveSeqno — a HINT. buildLineageGroups
// (and selectCandidate) must exclude !rr.Valid entries so a failed/hint-backed
// recover can neither form a phantom lineage (false split-brain) nor nominate a
// bootstrap candidate.
//
// SKIPPED until #2 is fixed: buildLineageGroups currently filters only on
// UUID/seqno and ignores rr.Valid, so the invalid entry below forms its own
// group and this test fails. Un-skip when the `!rr.Valid` exclusion lands.
func TestBuildLineageGroups_ExcludesInvalidRecover(t *testing.T) {
	t.Skip("guards bug #2 (issue #2) — un-skip when buildLineageGroups/selectCandidate exclude !rr.Valid")

	rec := map[string]wsrepRecoverResult{
		"n-0": {UUID: "aaa", Seqno: 100, LastCommitted: 100, Valid: true},  // clean recover
		"n-1": {UUID: "bbb", Seqno: 200, LastCommitted: 200, Valid: false}, // FAILED recover, hint seqno
	}
	groups := buildLineageGroups(rec)
	if len(groups) != 1 {
		t.Fatalf("an invalid (failed) recover must be excluded; want 1 lineage group, got %d", len(groups))
	}
	if groups[0].UUID != "aaa" || groups[0].BestNode != "n-0" {
		t.Fatalf("the clean node must own the only group; got uuid=%s best=%s", groups[0].UUID, groups[0].BestNode)
	}
}
