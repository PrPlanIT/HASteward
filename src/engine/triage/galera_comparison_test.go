package triage

import (
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

const cmpUUID = "b86ff01c-6dea-11f1-b59b-266ed2cb955a"

// quorumData builds a live 2-node Synced Primary quorum (c-0, c-1 at seqno 1000 on
// cmpUUID) plus a third node `third` whose position is unread — the kimai/osticket shape.
func quorumData(third grastate) *galeraTriageData {
	return &galeraTriageData{
		grastateData: []grastate{
			{Pod: "c-0", Source: "exec", UUID: cmpUUID},
			{Pod: "c-1", Source: "exec", UUID: cmpUUID},
			third,
		},
		effectiveSeqnos: map[string]*effectiveSeqno{
			"c-0": {Value: 1000, Known: true},
			"c-1": {Value: 1000, Known: true},
			// third absent → unread
		},
		wsrepMap: map[string]*wsrepStatus{
			"c-0": {ClusterStatus: "Primary", ClusterStateUUID: cmpUUID, LocalState: 4},
			"c-1": {ClusterStatus: "Primary", ClusterStateUUID: cmpUUID, LocalState: 4},
		},
		primaryMembers: []string{"c-0", "c-1"},
	}
}

// A live Synced Primary quorum + a same-lineage unread laggard (the kimai/osticket case)
// is PROVABLE, not undeterminable: the majority is authoritative and a down same-uuid node
// cannot have advanced past it, so it is recoverable by SST. Surfaced as a warning.
func TestGaleraComparison_QuorumLaggardIsRecoverable(t *testing.T) {
	tr := galeraTriageForTest()
	// c-2 unread, same lineage (uuid read from its grastate via probe).
	cmp := tr.crossInstanceComparison(quorumData(grastate{Pod: "c-2", Source: "pvc_probe", UUID: cmpUUID}))
	if !cmp.SafeToHeal || cmp.Authority != model.AuthorityProvable {
		t.Fatalf("clean quorum + same-lineage laggard must be Provable/safe, got %q safe=%v details=%v",
			cmp.Authority, cmp.SafeToHeal, cmp.SplitBrainDetails)
	}
	got := strings.Join(cmp.Warnings, " | ")
	if !strings.Contains(got, "c-2") || !strings.Contains(got, "recoverable") {
		t.Fatalf("recoverable laggard must be surfaced in warnings, got %v", cmp.Warnings)
	}
}

// A clean quorum does NOT excuse a node we cannot PLACE on its lineage: an unread node
// with an unknown UUID still fails closed (Undeterminable) — we can't prove it's behind.
func TestGaleraComparison_QuorumUnplaceableUnreadBlocks(t *testing.T) {
	tr := galeraTriageForTest()
	cmp := tr.crossInstanceComparison(quorumData(grastate{Pod: "c-2", Source: "none", UUID: "unknown"}))
	if cmp.SafeToHeal || cmp.Authority != model.AuthorityUndeterminable {
		t.Fatalf("an unplaceable (unknown-uuid) unread node must block → Undeterminable, got %q safe=%v",
			cmp.Authority, cmp.SafeToHeal)
	}
	if !containsAny(cmp.SplitBrainDetails, "UNREAD SEQNO") {
		t.Fatalf("blocking unread node must be named, got %v", cmp.SplitBrainDetails)
	}
}

// With NO live Primary quorum, even a same-uuid unread node cannot be proven behind
// anything → Undeterminable (the fail-closed default is unchanged).
func TestGaleraComparison_NoQuorumUnreadBlocks(t *testing.T) {
	tr := galeraTriageForTest()
	data := &galeraTriageData{
		grastateData:    []grastate{{Pod: "c-0", Source: "pvc_probe", UUID: cmpUUID}},
		effectiveSeqnos: map[string]*effectiveSeqno{}, // c-0 unread
		primaryMembers:  nil,
	}
	cmp := tr.crossInstanceComparison(data)
	if cmp.SafeToHeal || cmp.Authority != model.AuthorityUndeterminable {
		t.Fatalf("no quorum + unread must be Undeterminable, got %q safe=%v", cmp.Authority, cmp.SafeToHeal)
	}
}

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
