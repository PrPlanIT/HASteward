package bootstrap

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCandidateOverrideDiscardsData pins the pure data-loss guard: an override is
// unsafe exactly when the leader did NOT recover authoritatively AND its known seqno
// is strictly ahead of the best recovered node.
func TestCandidateOverrideDiscardsData(t *testing.T) {
	cases := []struct {
		name          string
		leaderRecAuth bool
		leaderKnown   int64
		bestRecovered int64
		wantDiscard   bool
	}{
		{"leader failed recover, known ahead -> unsafe", false, 100, 50, true},
		{"leader failed recover, known equal -> safe (no data past its position)", false, 50, 50, false},
		{"leader failed recover, known behind -> safe (best is genuinely ahead)", false, 30, 50, false},
		{"leader recovered authoritatively -> trust recovered comparison, safe", true, 100, 50, false},
		{"leader failed recover, known ahead by one -> unsafe", false, 51, 50, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := candidateOverrideDiscardsData(c.leaderRecAuth, c.leaderKnown, c.bestRecovered); got != c.wantDiscard {
				t.Fatalf("candidateOverrideDiscardsData(%v,%d,%d) = %v, want %v",
					c.leaderRecAuth, c.leaderKnown, c.bestRecovered, got, c.wantDiscard)
			}
		})
	}
}

func testGaleraBootstrap(t *testing.T, force bool) *galeraBootstrap {
	t.Helper()
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.mariadb.com/v1alpha1",
		"kind":       "MariaDB",
		"metadata":   map[string]interface{}{"name": "c", "namespace": "ns"},
		"spec":       map[string]interface{}{"replicas": int64(3)},
	}}
	p := provider.NewGaleraProviderForTest(
		&common.Config{ClusterName: "c", Namespace: "ns", Force: force}, 3, cr)
	return &galeraBootstrap{p: p}
}

const testUUID = "57a2c75c-6dea-11f1-b59b-266ed2cb955a"

// TestSelectCandidate_TransientRecoverFailureOnLeader is the P0 regression: triage
// proved c-0 the most-advanced (seqno 100), but c-0's wsrep_recover failed
// transiently (excluded from recovered), while c-1 recovered cleanly at seqno 50.
// Overriding to c-1 would SST c-0 from a node 50 transactions behind it — silent
// data loss. Without --force this MUST abort; with --force the operator owns the loss.
func TestSelectCandidate_TransientRecoverFailureOnLeader(t *testing.T) {
	assessments := []model.InstanceAssessment{
		{Pod: "c-0", EffectiveSeqno: 100},
		{Pod: "c-1", EffectiveSeqno: 50},
	}
	// Only c-1 recovered authoritatively; c-0's recover failed (absent from the map).
	recovered := map[string]wsrepRecoverResult{
		"c-1": {UUID: testUUID, Seqno: 50, LastCommitted: 50, Valid: true},
	}

	t.Run("without --force -> ABORT (fail closed)", func(t *testing.T) {
		b := testGaleraBootstrap(t, false)
		got, err := b.selectCandidate("c-0", assessments, recovered, &model.BootstrapResult{})
		if err == nil {
			t.Fatalf("expected an abort error; got candidate %q, nil error", got)
		}
	})

	t.Run("with --force -> operator accepts, proceeds to best recovered", func(t *testing.T) {
		b := testGaleraBootstrap(t, true)
		got, err := b.selectCandidate("c-0", assessments, recovered, &model.BootstrapResult{})
		if err != nil {
			t.Fatalf("with --force the override should proceed; got error %v", err)
		}
		if got != "c-1" {
			t.Fatalf("want candidate c-1 under --force, got %q", got)
		}
	})
}

// TestSelectCandidate_SafeOverrides: the guard must NOT block legitimate overrides.
func TestSelectCandidate_SafeOverrides(t *testing.T) {
	t.Run("leader recovered authoritatively but behind -> override is safe", func(t *testing.T) {
		assessments := []model.InstanceAssessment{
			{Pod: "c-0", EffectiveSeqno: 40},
			{Pod: "c-1", EffectiveSeqno: 50},
		}
		recovered := map[string]wsrepRecoverResult{
			"c-0": {UUID: testUUID, Seqno: 40, LastCommitted: 40, Valid: true},
			"c-1": {UUID: testUUID, Seqno: 50, LastCommitted: 50, Valid: true},
		}
		b := testGaleraBootstrap(t, false)
		got, err := b.selectCandidate("c-0", assessments, recovered, &model.BootstrapResult{})
		if err != nil {
			t.Fatalf("leader recovered and is genuinely behind — override should be safe; got %v", err)
		}
		if got != "c-1" {
			t.Fatalf("want c-1, got %q", got)
		}
	})

	t.Run("leader failed recover but was known BEHIND -> override is safe", func(t *testing.T) {
		assessments := []model.InstanceAssessment{
			{Pod: "c-0", EffectiveSeqno: 30}, // triage saw c-0 behind
			{Pod: "c-1", EffectiveSeqno: 50},
		}
		recovered := map[string]wsrepRecoverResult{
			"c-1": {UUID: testUUID, Seqno: 50, LastCommitted: 50, Valid: true},
		}
		b := testGaleraBootstrap(t, false)
		got, err := b.selectCandidate("c-0", assessments, recovered, &model.BootstrapResult{})
		if err != nil {
			t.Fatalf("leader was known behind — no data to lose, override should be safe; got %v", err)
		}
		if got != "c-1" {
			t.Fatalf("want c-1, got %q", got)
		}
	})
}
