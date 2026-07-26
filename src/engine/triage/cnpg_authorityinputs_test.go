package triage

import "testing"

// TestBuildAuthorityInputs_ReadStateBranches pins the fail-closed classification that
// decides whether a node's data is at risk. The load-bearing property: only POSITIVE
// proof (a mounted-and-empty volume, or a genuine NotFound PVC) makes a node
// AbsentNoData; a transient UNKNOWN PVC or an unstattable data dir must stay Unread,
// so a connectivity/mount blip can never discredit a data-bearing node.
func TestBuildAuthorityInputs_ReadStateBranches(t *testing.T) {
	data := &cnpgTriageData{
		controlData: []controlData{
			{Pod: "pg-1", Timeline: "1"},                                  // TL1, no history needed → Read
			{Pod: "pg-2", Timeline: "9", HistoryRaw: "8\t2C/99\tfork"},    // TL>1 with history → Read
			{Pod: "pg-3", Timeline: "9"},                                  // TL>1, no history → Unread (fork points unknown)
			{Pod: "pg-4", Timeline: "unknown", PGDataPresent: "empty"},    // provably empty → AbsentNoData
			{Pod: "pg-5", Timeline: "unknown"},                            // PVC MISSING → AbsentNoData
			{Pod: "pg-6", Timeline: "unknown"},                            // PVC UNKNOWN (transient) → Unread
			{Pod: "pg-7", Timeline: "unknown"},                            // PVC Bound, unread → Unread
			{Pod: "pg-8", Timeline: "unknown", PGDataPresent: "unknown"},  // unstattable dir → Unread
		},
		pvcStates: map[string]string{
			"pg-1": "Bound", "pg-2": "Bound", "pg-3": "Bound", "pg-4": "Bound",
			"pg-5": "MISSING", "pg-6": "UNKNOWN", "pg-7": "Bound", "pg-8": "Bound",
		},
	}
	want := map[string]ReadState{
		"pg-1": ReadStateRead,
		"pg-2": ReadStateRead,
		"pg-3": ReadStateUnread,
		"pg-4": ReadStateAbsentNoData,
		"pg-5": ReadStateAbsentNoData,
		"pg-6": ReadStateUnread, // transient UNKNOWN must NOT become AbsentNoData
		"pg-7": ReadStateUnread,
		"pg-8": ReadStateUnread,
	}

	inputs := buildAuthorityInputs(data, "pg-1")
	got := map[string]ReadState{}
	for _, in := range inputs {
		got[in.Pod] = in.ReadState
	}
	for pod, ws := range want {
		if got[pod] != ws {
			t.Errorf("%s: ReadState = %v, want %v", pod, got[pod], ws)
		}
	}
}
