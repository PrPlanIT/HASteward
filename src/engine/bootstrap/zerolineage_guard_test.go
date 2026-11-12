package bootstrap

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// The zero cluster UUID never establishes bootstrap authority, at any seqno, from a
// fenced recover or a live reading: zero groups are indistinguishable from each
// other, so trusting one blinds split-brain detection.

func TestIsAuthoritativeRecover_ZeroUUIDNever(t *testing.T) {
	for _, seqno := range []int64{0, 1, 15, 123702, 203767, maxPhantomSeqno} {
		zero := wsrepRecoverResult{UUID: zeroUUID, Seqno: seqno, LastCommitted: seqno, Valid: true}
		if isAuthoritativeRecover(zero) {
			t.Errorf("zero UUID at seqno %d is authoritative; it must never be", seqno)
		}
		realRR := wsrepRecoverResult{UUID: testUUID, Seqno: seqno, LastCommitted: seqno, Valid: true}
		if !isAuthoritativeRecover(realRR) {
			t.Errorf("control: real UUID at seqno %d is not authoritative", seqno)
		}
	}
}

// A belly-up cluster whose every fenced recover is zero yields no candidate, even when
// live readings taken before the fence put a node ahead, and even under --force.
func TestSelectCandidate_ZeroLineageNominatesNothing(t *testing.T) {
	assessments := []model.InstanceAssessment{
		{Pod: "c-0", EffectiveSeqno: -1},
		{Pod: "c-1", EffectiveSeqno: 203767},
		{Pod: "c-2", EffectiveSeqno: 203767},
	}
	recovered := map[string]wsrepRecoverResult{
		"c-0": {UUID: zeroUUID, Seqno: 200083, LastCommitted: 200083, Valid: true},
		"c-1": {UUID: zeroUUID, Seqno: 203767, LastCommitted: 203767, Valid: true},
		"c-2": {UUID: zeroUUID, Seqno: 203767, LastCommitted: 203767, Valid: true},
	}
	for _, force := range []bool{false, true} {
		res := &model.BootstrapResult{}
		got, err := testGaleraBootstrap(t, force).selectCandidate("", assessments, recovered, res)
		if err != nil {
			t.Fatalf("force=%v: unexpected error: %v", force, err)
		}
		if got != "" {
			t.Fatalf("force=%v: zero lineage nominated %q; want no candidate", force, got)
		}
		if len(res.Decision.LineageGroups) != 0 {
			t.Fatalf("force=%v: zero lineage formed lineage groups %+v; want none", force, res.Decision.LineageGroups)
		}
	}
}

// A zero-UUID node never overrides a real-lineage candidate, however far ahead it is.
func TestSelectCandidate_ZeroUUIDNeverOverridesRealLineage(t *testing.T) {
	assessments := []model.InstanceAssessment{
		{Pod: "c-1", EffectiveSeqno: 50},
		{Pod: "c-2", EffectiveSeqno: 203767},
	}
	recovered := map[string]wsrepRecoverResult{
		"c-1": {UUID: testUUID, Seqno: 50, LastCommitted: 50, Valid: true},
		"c-2": {UUID: zeroUUID, Seqno: 203767, LastCommitted: 203767, Valid: true},
	}
	for _, force := range []bool{false, true} {
		res := &model.BootstrapResult{}
		got, err := testGaleraBootstrap(t, force).selectCandidate("c-1", assessments, recovered, res)
		if err != nil {
			t.Fatalf("force=%v: unexpected error: %v", force, err)
		}
		if got != "c-1" {
			t.Fatalf("force=%v: candidate %q; the zero-UUID node must not override c-1", force, got)
		}
		if res.Decision.CandidateUUID == zeroUUID {
			t.Fatalf("force=%v: decision carries the zero UUID", force)
		}
	}
}
