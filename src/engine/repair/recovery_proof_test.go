package repair

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// deadlockTriage builds the calcom-shaped deadlock snapshot: authority -3,
// -1/-2 disposable, recovery blocked.
func deadlockTriage() *model.TriageResult {
	return &model.TriageResult{
		AuthorityStatus: "unambiguous",
		Recovery: &model.Recovery{
			Blocked:     true,
			Reason:      "disk_full_disposable_replica",
			Authority:   "calcom-postgres-3",
			Disposable:  []string{"calcom-postgres-1", "calcom-postgres-2"},
			RecoverySet: []string{"calcom-postgres-3", "calcom-postgres-1", "calcom-postgres-2"},
		},
		Assessments: []model.InstanceAssessment{
			{Pod: "calcom-postgres-3", IsPrimary: true, Timeline: 17, LSN: "B/8B000028", Classification: "authoritative"},
			{Pod: "calcom-postgres-1", Timeline: 16, LSN: "10/91000028", Classification: "disposable"},
			{Pod: "calcom-postgres-2", Timeline: 16, LSN: "E/24000060", Classification: "disposable"},
		},
	}
}

func validProof(t *model.TriageResult) RecoveryProof {
	return RecoveryProof{
		AssessmentHash: hashAssessment(t),
		Authority:      "calcom-postgres-3",
		Disposable:     []string{"calcom-postgres-1", "calcom-postgres-2"},
		EscrowRefs:     []escrow.EscrowRef{{Provider: "volumesnapshot", PVC: "calcom-postgres-1", Verified: true}},
		EscrowVerified: true,
	}
}

func TestRecoveryProofValid(t *testing.T) {
	tri := deadlockTriage()
	good := validProof(tri)

	if !good.Valid(tri) {
		t.Fatal("a complete proof against its own snapshot must be valid")
	}

	t.Run("hash drift (an instance advanced) invalidates", func(t *testing.T) {
		drift := deadlockTriage()
		drift.Assessments[1].Timeline = 17 // -1 advanced — no longer the same snapshot
		if good.Valid(drift) {
			t.Error("a changed snapshot must invalidate the proof")
		}
	})
	t.Run("authority change invalidates via the hash", func(t *testing.T) {
		drift := deadlockTriage()
		drift.Recovery.Authority = "calcom-postgres-1"
		if good.Valid(drift) {
			t.Error("an authority change must invalidate the proof")
		}
	})
	t.Run("empty authority invalidates", func(t *testing.T) {
		p := good
		p.Authority = ""
		if p.Valid(tri) {
			t.Error("empty authority must invalidate")
		}
	})
	t.Run("no disposable target invalidates", func(t *testing.T) {
		p := good
		p.Disposable = nil
		if p.Valid(tri) {
			t.Error("no disposable target must invalidate")
		}
	})
	t.Run("unverified escrow invalidates", func(t *testing.T) {
		p := good
		p.EscrowVerified = false
		if p.Valid(tri) {
			t.Error("unverified escrow must invalidate")
		}
	})
	t.Run("empty hash invalidates", func(t *testing.T) {
		p := good
		p.AssessmentHash = ""
		if p.Valid(tri) {
			t.Error("empty assessment hash must invalidate")
		}
	})
}

func TestAuthorizesClear(t *testing.T) {
	p := validProof(deadlockTriage())
	cases := []struct {
		inst string
		want bool
	}{
		{"calcom-postgres-1", true},  // disposable
		{"calcom-postgres-2", true},  // disposable
		{"calcom-postgres-3", false}, // AUTHORITY — must never be cleared
		{"calcom-postgres-9", false}, // not in the disposable set
		{"", false},
	}
	for _, c := range cases {
		if got := p.AuthorizesClear(c.inst); got != c.want {
			t.Errorf("AuthorizesClear(%q) = %v, want %v", c.inst, got, c.want)
		}
	}
}

func TestHashAssessmentDeterministicAndOrderIndependent(t *testing.T) {
	a := deadlockTriage()
	b := deadlockTriage()
	// Reorder assessments + disposable: the hash must be identical.
	b.Assessments[0], b.Assessments[2] = b.Assessments[2], b.Assessments[0]
	b.Recovery.Disposable = []string{"calcom-postgres-2", "calcom-postgres-1"}
	if hashAssessment(a) != hashAssessment(b) {
		t.Error("hash must be independent of slice ordering")
	}
	// A classification flip must change the hash.
	c := deadlockTriage()
	c.Assessments[1].Classification = "recoverable"
	if hashAssessment(a) == hashAssessment(c) {
		t.Error("a classification change must change the hash")
	}
}
