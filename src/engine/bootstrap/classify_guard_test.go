package bootstrap

import "testing"

// TestBuildLineageGroups_ExcludesInvalidRecover pins the fix for bug #2 (GitLab
// issue #2): when wsrep_recover FAILS for a node, executeBootstrap fabricates a
// Valid:false result carrying that node's triage EffectiveSeqno — a HINT.
// buildLineageGroups (via isAuthoritativeRecover) must exclude !rr.Valid entries
// so a failed/hint-backed recover can neither form a phantom lineage (false
// split-brain) nor nominate a bootstrap candidate.
func TestBuildLineageGroups_ExcludesInvalidRecover(t *testing.T) {
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

// TestBuildLineageGroups_ExcludesUnknownUUID pins bug #4 (GitLab issue #4): triage
// assigns UUID="unknown" to no-data / wedged nodes. Even a Valid result carrying
// "unknown" must not form a lineage — otherwise it manufactures a phantom
// split-brain or a bogus candidate.
func TestBuildLineageGroups_ExcludesUnknownUUID(t *testing.T) {
	rec := map[string]wsrepRecoverResult{
		"n-0": {UUID: "aaa", Seqno: 100, LastCommitted: 100, Valid: true},
		"n-1": {UUID: "unknown", Seqno: 200, LastCommitted: 200, Valid: true}, // wedged/no-data sentinel
	}
	groups := buildLineageGroups(rec)
	if len(groups) != 1 || groups[0].UUID != "aaa" {
		t.Fatalf(`"unknown" UUID must be excluded; want only lineage aaa, got %+v`, groups)
	}
}
