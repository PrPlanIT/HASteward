package triage

import (
	"strings"
	"testing"
)

// TestCnpgCrossInstanceComparison pins the authority/split-brain determination —
// in particular the stale-restore case that must NOT be declared safe to heal.
//
// Regression: boundary-postgres. A restore/PITR created a HIGHER timeline number
// (9) from an OLDER LSN, so the restored primary (timeline 9, LSN 2E/B9000070)
// carried ~4 months-stale data while the crash-looping replica (timeline 8, LSN
// 8F/25000028) held the real recent data. The old "highest timeline wins"
// heuristic reported SafeToHeal=true and would have re-cloned the good replica
// from the stale primary — silent, unrecoverable data loss.
func TestCnpgCrossInstanceComparison(t *testing.T) {
	mk := func(pod, tl, lsn string) controlData {
		return controlData{Pod: pod, Timeline: tl, CheckpointLocation: lsn, Source: "exec", Reachable: true}
	}

	tests := []struct {
		name         string
		primary      string
		primaryTL    string
		instances    []controlData // includes the primary
		wantSafe     bool
		wantAdvanced string // MostAdvanced pod (only asserted when non-empty)
		wantDetail   string // substring expected in a split-brain detail (when unsafe)
	}{
		{
			// THE bug: older timeline but LSN far ahead of the (stale-restore) primary.
			name:      "stale-restore primary: older-timeline replica ahead by LSN -> UNSAFE",
			primary:   "boundary-postgres-3",
			primaryTL: "9",
			instances: []controlData{
				mk("boundary-postgres-3", "9", "2E/B9000070"),
				mk("boundary-postgres-2", "8", "8F/25000028"),
			},
			wantSafe:     false,
			wantAdvanced: "boundary-postgres-2",
			wantDetail:   "stale restore",
		},
		{
			// The legitimate case the fix must NOT break: an old-timeline replica that
			// is genuinely behind (LSN before the fork) stays safe to heal.
			name:      "old-timeline replica genuinely behind -> SAFE",
			primary:   "pg-1",
			primaryTL: "9",
			instances: []controlData{
				mk("pg-1", "9", "50/00000000"),
				mk("pg-2", "8", "40/00000000"),
			},
			wantSafe:     true,
			wantAdvanced: "pg-1",
		},
		{
			// Pre-existing detection must still fire: replica on a HIGHER timeline.
			name:      "replica on higher timeline -> UNSAFE",
			primary:   "pg-1",
			primaryTL: "8",
			instances: []controlData{
				mk("pg-1", "8", "40/00000000"),
				mk("pg-2", "9", "41/00000000"),
			},
			wantSafe:     false,
			wantAdvanced: "pg-2",
			wantDetail:   "timeline 9 > primary timeline 8",
		},
		{
			// Pre-existing detection must still fire: same timeline, LSN ahead.
			name:      "same timeline, replica LSN ahead -> UNSAFE",
			primary:   "pg-1",
			primaryTL: "9",
			instances: []controlData{
				mk("pg-1", "9", "50/00000000"),
				mk("pg-2", "9", "60/00000000"),
			},
			wantSafe:   false,
			wantDetail: "ahead of primary",
		},
		{
			// Healthy: same timeline, replica behind (normal streaming lag) -> SAFE.
			name:      "same timeline, replica behind -> SAFE",
			primary:   "pg-1",
			primaryTL: "9",
			instances: []controlData{
				mk("pg-1", "9", "60/00000000"),
				mk("pg-2", "9", "50/00000000"),
			},
			wantSafe:     true,
			wantAdvanced: "pg-1",
		},
		{
			// Equal LSN on an older timeline is at-or-before the fork -> SAFE
			// (boundary condition: strictly-greater is what proves divergence).
			name:      "older timeline, LSN equal to primary -> SAFE",
			primary:   "pg-1",
			primaryTL: "9",
			instances: []controlData{
				mk("pg-1", "9", "50/00000000"),
				mk("pg-2", "8", "50/00000000"),
			},
			wantSafe: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := &cnpgTriageData{
				controlData:     tt.instances,
				primaryTimeline: tt.primaryTL,
			}
			for i := range data.controlData {
				if data.controlData[i].Pod == tt.primary {
					data.primaryControlData = &data.controlData[i]
				}
			}
			cmp := cnpgCrossInstanceComparison(data, tt.primary)

			if cmp.SafeToHeal != tt.wantSafe {
				t.Errorf("SafeToHeal = %v, want %v (details: %v)", cmp.SafeToHeal, tt.wantSafe, cmp.SplitBrainDetails)
			}
			if tt.wantAdvanced != "" && cmp.MostAdvanced != tt.wantAdvanced {
				t.Errorf("MostAdvanced = %q, want %q", cmp.MostAdvanced, tt.wantAdvanced)
			}
			if !tt.wantSafe {
				if len(cmp.SplitBrainDetails) == 0 {
					t.Fatalf("expected split-brain details, got none")
				}
				if tt.wantDetail != "" && !containsAny(cmp.SplitBrainDetails, tt.wantDetail) {
					t.Errorf("split-brain details %v, want a substring %q", cmp.SplitBrainDetails, tt.wantDetail)
				}
			}
		})
	}
}

// TestCnpgCrossInstanceComparison_FailClosedUnread ports Galera's "bug #1" guard
// to CNPG: a node whose PVC is Bound (data on disk) but whose pg_controldata could
// not be read must make SafeToHeal false — the tool must never declare a heal
// target while blind to a node that might hold the most-advanced history. A node
// with no Bound PVC has nothing to lose and does not block.
func TestCnpgCrossInstanceComparison_FailClosedUnread(t *testing.T) {
	unreadable := func(pod string) controlData {
		return controlData{Pod: pod, Timeline: "unknown", CheckpointLocation: "unknown", Source: "none"}
	}
	readable := func(pod, tl, lsn string) controlData {
		return controlData{Pod: pod, Timeline: tl, CheckpointLocation: lsn, Source: "exec", Reachable: true}
	}

	t.Run("Bound PVC but unread position -> UNSAFE (fail closed)", func(t *testing.T) {
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "9", "50/00000000"), unreadable("pg-2")},
			primaryTimeline: "9",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "Bound"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if cmp.SafeToHeal {
			t.Fatalf("SafeToHeal=true while blind to a Bound-PVC node; want false. details=%v", cmp.SplitBrainDetails)
		}
		if !containsAny(cmp.SplitBrainDetails, "UNREAD") {
			t.Fatalf("expected an UNREAD flag, got %v", cmp.SplitBrainDetails)
		}
	})

	t.Run("unread position but NO Bound PVC -> SAFE (nothing to lose)", func(t *testing.T) {
		data := &cnpgTriageData{
			controlData:     []controlData{readable("pg-1", "9", "50/00000000"), unreadable("pg-2")},
			primaryTimeline: "9",
			pvcStates:       map[string]string{"pg-1": "Bound", "pg-2": "MISSING"},
		}
		data.primaryControlData = &data.controlData[0]
		cmp := cnpgCrossInstanceComparison(data, "pg-1")
		if !cmp.SafeToHeal {
			t.Fatalf("SafeToHeal=false for an absent (no Bound PVC) node; want true. details=%v", cmp.SplitBrainDetails)
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
