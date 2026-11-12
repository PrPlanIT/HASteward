package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/engine/provider"
)

// A live Primary on the zero UUID vouches for no unread node: its lineage can't be
// told apart from another zero group, so the unread node still blocks any heal.
func TestCrossInstanceComparison_ZeroLineageQuorumVouchesForNoOne(t *testing.T) {
	tr := &galeraTriage{}
	ws := func() *wsrepStatus {
		return &wsrepStatus{ClusterStateUUID: provider.ZeroUUID, ClusterStatus: "Primary", LocalState: 4, LastCommitted: 203767}
	}
	data := &galeraTriageData{
		grastateData: []grastate{
			{Pod: "n-0", UUID: provider.ZeroUUID},
			{Pod: "n-1", UUID: provider.ZeroUUID},
			{Pod: "n-2", UUID: provider.ZeroUUID},
		},
		wsrepMap:       map[string]*wsrepStatus{"n-1": ws(), "n-2": ws()},
		primaryMembers: []string{"n-1", "n-2"},
		effectiveSeqnos: map[string]*effectiveSeqno{
			"n-0": {Value: -1, Source: "none", Known: false},
			"n-1": {Value: 203767, Source: "wsrep_last_committed", Known: true},
			"n-2": {Value: 203767, Source: "wsrep_last_committed", Known: true},
		},
	}
	cmp := tr.crossInstanceComparison(data)
	if cmp.SafeToHeal {
		t.Fatalf("SafeToHeal=true behind a zero-UUID quorum; want false. details=%v", cmp.SplitBrainDetails)
	}
	if !containsSub(cmp.SplitBrainDetails, "UNREAD") {
		t.Fatalf("n-0 must stay an UNREAD blocker; got %v", cmp.SplitBrainDetails)
	}
	if containsSub(cmp.Warnings, "recoverable by SST") {
		t.Fatalf("a zero-UUID quorum vouched for n-0 as recoverable: %v", cmp.Warnings)
	}

	// Control: the same shape on a real UUID does vouch for n-0.
	const realUUID = "4385a998-d4b2-11f1-b872-5a19d1251c10"
	for _, w := range data.wsrepMap {
		w.ClusterStateUUID = realUUID
	}
	for i := range data.grastateData {
		data.grastateData[i].UUID = realUUID
	}
	if cmp := tr.crossInstanceComparison(data); !containsSub(cmp.Warnings, "recoverable by SST") {
		t.Fatalf("control: a real-UUID quorum should vouch for n-0; warnings=%v details=%v", cmp.Warnings, cmp.SplitBrainDetails)
	}
}
