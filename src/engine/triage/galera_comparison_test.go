package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

const cmpUUID = "b86ff01c-6dea-11f1-b59b-266ed2cb955a"

// A non-primary node with a KNOWN seqno ahead of the primary component's best is a
// split-brain: not safe to heal, and the shared outcome is Diverged.
func TestGaleraComparison_SeqnoAheadOfPrimaryDiverges(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData: []grastate{
			{Pod: "c-0", Source: "exec", UUID: cmpUUID},
			{Pod: "c-1", Source: "exec", UUID: cmpUUID},
		},
		effectiveSeqnos: map[string]*effectiveSeqno{
			"c-0": {Value: 50, Known: true},
			"c-1": {Value: 100, Known: true}, // ahead, and NOT in the primary component
		},
		primaryMembers: []string{"c-0"},
	}
	cmp := tr.crossInstanceComparison(data)
	if cmp.SafeToHeal {
		t.Fatalf("a node ahead of the primary is split-brain; details=%v", cmp.SplitBrainDetails)
	}
	if cmp.Authority != model.AuthorityDiverged {
		t.Fatalf("want Diverged, got %q (details=%v)", cmp.Authority, cmp.SplitBrainDetails)
	}
}

// Precedence: an UNREAD node dominates divergence evidence — it must be recovered
// before anything is provable, so the outcome is Undeterminable even when a
// seqno-ahead divergence is also present.
func TestGaleraComparison_UnreadDominatesDivergence(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData: []grastate{
			{Pod: "c-0", Source: "exec", UUID: cmpUUID},
			{Pod: "c-1", Source: "exec", UUID: cmpUUID},
			{Pod: "c-2", Source: "exec", UUID: cmpUUID}, // unread: no Known effective seqno
		},
		effectiveSeqnos: map[string]*effectiveSeqno{
			"c-0": {Value: 50, Known: true},
			"c-1": {Value: 100, Known: true}, // divergence present
			// c-2 absent → unread
		},
		primaryMembers: []string{"c-0"},
	}
	cmp := tr.crossInstanceComparison(data)
	if cmp.SafeToHeal {
		t.Fatal("unread + divergence must not be safe")
	}
	if cmp.Authority != model.AuthorityUndeterminable {
		t.Fatalf("unread must dominate → Undeterminable, got %q (details=%v)", cmp.Authority, cmp.SplitBrainDetails)
	}
}
