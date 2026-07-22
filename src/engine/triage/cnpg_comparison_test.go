package triage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// histTL builds the raw <n>.history file content for an instance, where forks[i] is
// the LSN at which timeline (i+2) branched from timeline (i+1). An instance on
// timeline N has N-1 fork lines (parent timelines 1..N-1).
func histTL(forks ...string) string {
	var b strings.Builder
	for i, f := range forks {
		fmt.Fprintf(&b, "%d\t%s\tfork\n", i+1, f)
	}
	return b.String()
}

// sharedForks describes history up to timeline 8 (7 fork lines) — the common prefix
// a TL8 replica and a TL9 primary share in the stale-restore scenarios.
var sharedForks = []string{"0/100", "0/200", "0/300", "0/400", "0/500", "0/600", "0/700"}

// TestCnpgCrossInstanceComparison pins the lineage-aware authority determination,
// most importantly the boundary-postgres stale-restore family. A restore lands on a
// HIGHER timeline NUMBER from an OLDER fork point; "highest timeline wins" would have
// re-cloned the good replica from the stale primary — silent, unrecoverable loss.
func TestCnpgCrossInstanceComparison(t *testing.T) {
	// cd builds a readable instance with its timeline lineage.
	cd := func(pod, tl, lsn, history string) controlData {
		return controlData{Pod: pod, Timeline: tl, CheckpointLocation: lsn, HistoryRaw: history,
			Source: "exec", Reachable: true, PGDataPresent: "yes"}
	}

	tests := []struct {
		name          string
		primary       string
		instances     []controlData
		wantSafe      bool
		wantAuthority model.AuthorityOutcome
		wantAdvanced  string // MostAdvanced pod, asserted when non-empty
		wantDetail    string // substring expected in SplitBrainDetails (when unsafe)
	}{
		{
			// Boundary shape: the restored primary (TL9) ALSO wrote past the fork, and
			// the golden replica (TL8) has its own writes past it → true divergence, no
			// safe winner. Refuse and preserve both.
			name:    "stale-restore, both past fork -> DIVERGED",
			primary: "boundary-postgres-3",
			instances: []controlData{
				cd("boundary-postgres-3", "9", "2E/B9000070", histTL(append(append([]string{}, sharedForks...), "2C/99000000")...)),
				cd("boundary-postgres-2", "8", "8F/25000028", histTL(sharedForks...)),
			},
			wantSafe:      false,
			wantAuthority: model.AuthorityDiverged,
			wantDetail:    "shared fork",
		},
		{
			// The decisive stale-restore: the restore sits AT the fork with no writes;
			// the lower-timeline replica holds the real data → replica wins, decisively.
			name:    "stale-restore, restore dataless past fork -> replica leads",
			primary: "pg-1",
			instances: []controlData{
				cd("pg-1", "9", "2C/99000000", histTL(append(append([]string{}, sharedForks...), "2C/99000000")...)),
				cd("pg-2", "8", "8F/25000028", histTL(sharedForks...)),
			},
			wantSafe:      false, // leader is not the primary → no heal-from-primary
			wantAuthority: model.AuthorityLeaderNotPrimary,
			wantAdvanced:  "pg-2",
			wantDetail:    "AUTHORITY IS NOT THE PRIMARY",
		},
		{
			// Legitimate case the fix must NOT break: an old-timeline replica genuinely
			// behind (before the fork) is a pure ancestor → primary leads, safe to heal.
			name:    "old-timeline replica behind -> SAFE",
			primary: "pg-1",
			instances: []controlData{
				cd("pg-1", "9", "55/00000000", histTL(append(append([]string{}, sharedForks...), "50/00000000")...)),
				cd("pg-2", "8", "40/00000000", histTL(sharedForks...)),
			},
			wantSafe:      true,
			wantAuthority: model.AuthorityProvable,
			wantAdvanced:  "pg-1",
		},
		{
			// Replica genuinely ahead on a NEW timeline (a promoted/advanced replica) →
			// decisive leader is that replica, not the primary → refuse heal-from-primary.
			name:    "replica ahead on higher timeline -> leader not primary",
			primary: "pg-1",
			instances: []controlData{
				cd("pg-1", "8", "40/00000000", histTL(sharedForks...)),
				cd("pg-2", "9", "41/00000000", histTL(append(append([]string{}, sharedForks...), "40/00000000")...)),
			},
			wantSafe:      false,
			wantAuthority: model.AuthorityLeaderNotPrimary,
			wantAdvanced:  "pg-2",
		},
		{
			// Same timeline, replica behind (normal streaming lag) → primary leads, SAFE.
			name:    "same timeline, replica behind -> SAFE",
			primary: "pg-1",
			instances: []controlData{
				cd("pg-1", "9", "60/00000000", histTL(append(append([]string{}, sharedForks...), "50/00000000")...)),
				cd("pg-2", "9", "50/00000000", histTL(append(append([]string{}, sharedForks...), "50/00000000")...)),
			},
			wantSafe:      true,
			wantAuthority: model.AuthorityProvable,
			wantAdvanced:  "pg-1",
		},
		{
			// Older timeline, checkpoint exactly AT the fork → at-or-before the fork, a
			// pure ancestor → SAFE (strictly-greater is what proves divergence).
			name:    "older timeline, LSN equal to fork -> SAFE",
			primary: "pg-1",
			instances: []controlData{
				cd("pg-1", "9", "50/00000000", histTL(append(append([]string{}, sharedForks...), "50/00000000")...)),
				cd("pg-2", "8", "50/00000000", histTL(sharedForks...)),
			},
			wantSafe:      true,
			wantAuthority: model.AuthorityProvable,
			wantAdvanced:  "pg-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := &cnpgTriageData{controlData: tt.instances, pvcStates: map[string]string{}}
			for i := range data.controlData {
				data.pvcStates[data.controlData[i].Pod] = "Bound"
				if data.controlData[i].Pod == tt.primary {
					data.primaryControlData = &data.controlData[i]
					data.primaryTimeline = data.controlData[i].Timeline
				}
			}
			cmp := cnpgCrossInstanceComparison(data, tt.primary)

			if cmp.SafeToHeal != tt.wantSafe {
				t.Errorf("SafeToHeal = %v, want %v (details: %v)", cmp.SafeToHeal, tt.wantSafe, cmp.SplitBrainDetails)
			}
			if cmp.Authority != tt.wantAuthority {
				t.Errorf("Authority = %q, want %q (details: %v)", cmp.Authority, tt.wantAuthority, cmp.SplitBrainDetails)
			}
			if tt.wantAdvanced != "" && cmp.MostAdvanced != tt.wantAdvanced {
				t.Errorf("MostAdvanced = %q, want %q", cmp.MostAdvanced, tt.wantAdvanced)
			}
			if tt.wantDetail != "" && !containsAny(cmp.SplitBrainDetails, tt.wantDetail) {
				t.Errorf("SplitBrainDetails %v, want a substring %q", cmp.SplitBrainDetails, tt.wantDetail)
			}
		})
	}
}

// TestCnpgCrossInstanceComparison_FailClosedUnread pins the legibility gate: a node
// that could hold data but was not read makes authority UNDETERMINABLE — never a
// silent "safe to heal". A node with no data to lose does not block.
func TestCnpgCrossInstanceComparison_FailClosedUnread(t *testing.T) {
	// A timeline-1 readable primary needs no history.
	readable := func(pod, tl, lsn string) controlData {
		return controlData{Pod: pod, Timeline: tl, CheckpointLocation: lsn, Source: "exec",
			Reachable: true, PGDataPresent: "yes"}
	}
	unreadable := func(pod string) controlData {
		return controlData{Pod: pod, Timeline: "unknown", CheckpointLocation: "unknown", Source: "none"}
	}

	t.Run("Bound PVC but unread position -> UNDETERMINABLE", func(t *testing.T) {
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "1", "50/00000000"), unreadable("pg-2")},
			primaryTimeline: "1",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "Bound"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if cmp.SafeToHeal || cmp.Authority != model.AuthorityUndeterminable {
			t.Fatalf("want unsafe+undeterminable while blind to a Bound-PVC node; got safe=%v authority=%q", cmp.SafeToHeal, cmp.Authority)
		}
		if !containsAny(cmp.SplitBrainDetails, "UNREAD") {
			t.Fatalf("expected an UNREAD blocker, got %v", cmp.SplitBrainDetails)
		}
	})

	t.Run("PVC Pending and unread -> UNDETERMINABLE (not silently absent)", func(t *testing.T) {
		// Generalization beyond Bound: a Pending/Lost/Released PVC may still hold data.
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "1", "50/00000000"), unreadable("pg-2")},
			primaryTimeline: "1",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "Pending"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if cmp.Authority != model.AuthorityUndeterminable {
			t.Fatalf("a Pending (present-but-unmountable) PVC must block, not be treated as absent: authority=%q details=%v", cmp.Authority, cmp.SplitBrainDetails)
		}
	})

	t.Run("no PVC -> nothing to lose -> SAFE", func(t *testing.T) {
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "1", "50/00000000"), unreadable("pg-2")},
			primaryTimeline: "1",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "MISSING"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if !cmp.SafeToHeal {
			t.Fatalf("SafeToHeal=false for an absent (no PVC) node; want true. details=%v", cmp.SplitBrainDetails)
		}
	})

	t.Run("mounted but provably-empty volume -> nothing to lose -> SAFE", func(t *testing.T) {
		empty := controlData{Pod: "pg-2", Timeline: "unknown", Source: "pvc_probe", PGDataPresent: "empty"}
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "1", "50/00000000"), empty},
			primaryTimeline: "1",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "Bound"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if !cmp.SafeToHeal {
			t.Fatalf("a provably-empty Bound volume should not block; got details=%v", cmp.SplitBrainDetails)
		}
	})

	// The scenarios that MUST fail closed: a transient connectivity/mount failure must
	// produce Unread (refuse), never AbsentNoData — it can never discredit a node that
	// might hold the winning data.
	t.Run("transient PVC API error (UNKNOWN) -> UNDETERMINABLE, never absent", func(t *testing.T) {
		unread := controlData{Pod: "pg-2", Timeline: "unknown", Source: "none"}
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "1", "50/00000000"), unread},
			primaryTimeline: "1",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "UNKNOWN"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if cmp.SafeToHeal || cmp.Authority != model.AuthorityUndeterminable {
			t.Fatalf("a transient PVC-Get error must block, not be read as absent; got safe=%v authority=%q", cmp.SafeToHeal, cmp.Authority)
		}
	})

	t.Run("ambiguous data-dir probe (unknown) -> UNDETERMINABLE, never absent", func(t *testing.T) {
		// The probe ran but couldn't stat pgdata (mount glitch / permission) → "unknown",
		// NOT "empty". Must block, never be treated as a disposable empty volume.
		amb := controlData{Pod: "pg-2", Timeline: "unknown", Source: "pvc_probe", PGDataPresent: "unknown"}
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "1", "50/00000000"), amb},
			primaryTimeline: "1",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "Bound"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if cmp.Authority != model.AuthorityUndeterminable {
			t.Fatalf("an unknown (unstattable) data dir must block, not be treated as empty; authority=%q details=%v", cmp.Authority, cmp.SplitBrainDetails)
		}
	})
}

func containsAny(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
