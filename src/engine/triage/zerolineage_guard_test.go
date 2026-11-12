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

// effectiveFromReadings derives each node's effective seqno exactly as the gather does.
func effectiveFromReadings(data *galeraTriageData) {
	data.effectiveSeqnos = map[string]*effectiveSeqno{}
	for _, gs := range data.grastateData {
		ws, gsSeqno := lineagePositions(gs, data.wsrepMap[gs.Pod])
		es := deriveEffectiveSeqno(ws, -1, -1, gsSeqno, -1, -1)
		data.effectiveSeqnos[gs.Pod] = &es
	}
}

// A position on a non-lineage UUID is never read as a position, live or on disk.
func TestLineagePositions_ZeroUUIDNeverAPosition(t *testing.T) {
	for _, u := range []string{provider.ZeroUUID, "", "unknown"} {
		ws, gs := lineagePositions(grastate{Pod: "n-0", UUID: u, Seqno: "203767"},
			&wsrepStatus{ClusterStateUUID: u, LastCommitted: 203767})
		if ws != -1 || gs != -1 {
			t.Errorf("uuid %q: positions live=%d grastate=%d; want -1/-1", u, ws, gs)
		}
	}
	const realUUID = "4385a998-d4b2-11f1-b872-5a19d1251c10"
	ws, gs := lineagePositions(grastate{Pod: "n-0", UUID: realUUID, Seqno: "203767"},
		&wsrepStatus{ClusterStateUUID: realUUID, LastCommitted: 203767})
	if ws != 203767 || gs != 203767 {
		t.Fatalf("control: real uuid positions live=%d grastate=%d; want 203767/203767", ws, gs)
	}
}

// A cleanly stopped zero-lineage cluster (every grastate 00000000:N) is not a provable
// authority: no node is Known, so heal and bootstrap are refused.
func TestCrossInstanceComparison_CleanStopZeroLineageUndeterminable(t *testing.T) {
	tr := &galeraTriage{}
	build := func(uuid string) *galeraTriageData {
		data := &galeraTriageData{
			grastateData: []grastate{
				{Pod: "n-0", UUID: uuid, Seqno: "200083"},
				{Pod: "n-1", UUID: uuid, Seqno: "203767"},
				{Pod: "n-2", UUID: uuid, Seqno: "203767"},
			},
			wsrepMap: map[string]*wsrepStatus{},
		}
		effectiveFromReadings(data)
		return data
	}
	data := build(provider.ZeroUUID)
	for pod, es := range data.effectiveSeqnos {
		if es.Known {
			t.Fatalf("%s: zero-UUID grastate read as Known %+v", pod, es)
		}
	}
	if cmp := tr.crossInstanceComparison(data); cmp.SafeToHeal || !containsSub(cmp.SplitBrainDetails, "UNREAD") {
		t.Fatalf("clean-stop zero lineage: SafeToHeal=%v details=%v; want refused as UNREAD", cmp.SafeToHeal, cmp.SplitBrainDetails)
	}

	// Control: the same clean stop on a real lineage is provable.
	if cmp := tr.crossInstanceComparison(build("4385a998-d4b2-11f1-b872-5a19d1251c10")); !cmp.SafeToHeal {
		t.Fatalf("control: real-lineage clean stop should be provable; details=%v", cmp.SplitBrainDetails)
	}
}

// A live zero-lineage Primary with every node Synced is not healthy: it is not a
// provable authority, so repair can't SST a joiner from it.
func TestCrossInstanceComparison_LiveZeroLineageNotSafeToHeal(t *testing.T) {
	tr := &galeraTriage{}
	build := func(uuid string) *galeraTriageData {
		ws := func() *wsrepStatus {
			return &wsrepStatus{ClusterStateUUID: uuid, ClusterStatus: "Primary", LocalState: 4,
				LocalStateComment: "Synced", LastCommitted: 203767}
		}
		data := &galeraTriageData{
			grastateData: []grastate{
				{Pod: "n-0", UUID: uuid, Seqno: "-1"},
				{Pod: "n-1", UUID: uuid, Seqno: "-1"},
				{Pod: "n-2", UUID: uuid, Seqno: "-1"},
			},
			wsrepMap:       map[string]*wsrepStatus{"n-0": ws(), "n-1": ws(), "n-2": ws()},
			primaryMembers: []string{"n-0", "n-1", "n-2"},
		}
		effectiveFromReadings(data)
		return data
	}
	if cmp := tr.crossInstanceComparison(build(provider.ZeroUUID)); cmp.SafeToHeal {
		t.Fatalf("live zero-lineage Primary read as SafeToHeal; details=%v", cmp.SplitBrainDetails)
	}

	// Control: the same live Primary on a real lineage is healthy.
	if cmp := tr.crossInstanceComparison(build("4385a998-d4b2-11f1-b872-5a19d1251c10")); !cmp.SafeToHeal {
		t.Fatalf("control: real-lineage live Primary should be SafeToHeal; details=%v", cmp.SplitBrainDetails)
	}
}
