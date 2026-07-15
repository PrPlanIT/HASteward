package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// TestClassify_Scenarios is the Tier-1 scenario suite: a table of named, synthesized
// cluster states asserting the auto-repairable-vs-operator verdict. Each row is a
// case we actually see (or must never mishandle); as new recurrent cases show up in
// production, add a row. Pure — no cluster, no k8s.
func TestClassify_Scenarios(t *testing.T) {
	// safe builds a SafeToHeal DataComparison with the given Primary members.
	safe := func(primary ...string) model.DataComparison {
		return model.DataComparison{SafeToHeal: true, PrimaryMembers: primary}
	}
	pods := func(names ...string) []model.InstanceAssessment {
		out := make([]model.InstanceAssessment, len(names))
		for i, n := range names {
			out[i] = model.InstanceAssessment{Pod: n}
		}
		return out
	}

	tests := []struct {
		name        string
		r           *model.TriageResult
		want        ClusterVerdict
		wantTargets []string
	}{
		{
			"all synced, nothing to do -> healthy",
			&model.TriageResult{DataComparison: safe("n-0", "n-1", "n-2"), Assessments: pods("n-0", "n-1", "n-2")},
			VerdictHealthy, nil,
		},
		{
			"healthy cluster + stale operator-snapshot UUID -> healthy (NOT a false split-brain)",
			// The osticket normal-day case: crossInstanceComparison already keeps
			// SafeToHeal=true, so the top-level verdict is plainly healthy.
			&model.TriageResult{DataComparison: safe("n-0", "n-1", "n-2"), Assessments: pods("n-0", "n-1", "n-2")},
			VerdictHealthy, nil,
		},
		{
			"one node crashlooped, 2 Synced Primary -> auto-heal (the repairable hiccup)",
			&model.TriageResult{
				DataComparison: safe("n-0", "n-1"),
				Assessments: []model.InstanceAssessment{
					{Pod: "n-0"}, {Pod: "n-1"}, {Pod: "n-2", NeedsHeal: true},
				},
			},
			VerdictAutoHeal, []string{"n-2"},
		},
		{
			"belly-up, clean single lineage recovered (SafeToHeal, no primary) -> bootstrap-required",
			&model.TriageResult{
				AllNodesDown:   true,
				DataComparison: model.DataComparison{SafeToHeal: true}, // authoritative positions, one UUID; nobody serving
				Assessments:    pods("n-0", "n-1", "n-2"),
			},
			VerdictBootstrapRequired, nil,
		},
		{
			"belly-up, divergent lineage / unread (no primary, !SafeToHeal) -> operator-required",
			&model.TriageResult{
				AllNodesDown: true,
				DataComparison: model.DataComparison{
					SafeToHeal:        false,
					SplitBrainDetails: []string{"Multiple cluster UUIDs detected: a, b"},
				},
			},
			VerdictOperatorRequired, nil,
		},
		{
			"running but no Primary component, positions unread (!SafeToHeal) -> operator-required",
			&model.TriageResult{
				AllNodesDown: false,
				DataComparison: model.DataComparison{
					SafeToHeal:        false,
					SplitBrainDetails: []string{"UNREAD SEQNO — authority undeterminable for: n-0, n-1"},
				},
				Assessments: []model.InstanceAssessment{{Pod: "n-0", IsRunning: true}, {Pod: "n-1", IsRunning: true}},
			},
			VerdictOperatorRequired, nil,
		},
		{
			"split-brain WITH a live primary (!SafeToHeal) -> operator-required",
			&model.TriageResult{
				DataComparison: model.DataComparison{
					SafeToHeal:        false,
					PrimaryMembers:    []string{"n-0"},
					SplitBrainDetails: []string{"n-1 has seqno 200 > primary best 100 (n-0)"},
				},
				Assessments: []model.InstanceAssessment{{Pod: "n-0"}, {Pod: "n-1", NeedsHeal: true}},
			},
			VerdictOperatorRequired, nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.r)
			if got.Class != tt.want {
				t.Fatalf("Classify = %q (%s), want %q", got.Class, got.Reason, tt.want)
			}
			if !strsEqual(got.Targets, tt.wantTargets) {
				t.Fatalf("targets = %v, want %v", got.Targets, tt.wantTargets)
			}
		})
	}
}

func strsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
