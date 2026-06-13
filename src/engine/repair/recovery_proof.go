package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// RecoveryProof is the typed gate the deadlock-breaker MUST satisfy before it
// clears any datadir. It binds the destructive decision to a SPECIFIC triage
// snapshot (AssessmentHash) and to a verified escrow (EscrowVerified), so
// "the cluster has not changed since we decided" and "we proved we can roll
// back" are enforceable invariants, not conventions. The breaker assembles it
// after escrow verification, re-triages at the destructive edge, and proceeds
// only if Valid(currentTriage) holds.
type RecoveryProof struct {
	AssessmentHash string             // hash of the triage snapshot this proof was derived from
	Authority      string             // the authoritative pod that must NOT be cleared
	Disposable     []string           // pods proven disposable (the only clear targets)
	EscrowRefs     []escrow.EscrowRef // verified escrow protecting the recovery set
	EscrowVerified bool               // escrow.Verify proved restorability
}

// Valid reports whether the proof still authorizes a destructive clear against
// the CURRENT triage snapshot. It is the single gate: the assessment must be
// byte-identical to the one the proof was built from (no state drift since the
// decision), authority must be known, there must be a disposable target, and
// escrow must be verified. A false return MUST abort the breaker.
//
// Because AssessmentHash covers authority and the disposable set, a hash match
// is what makes "re-confirm authority unchanged" a typed invariant rather than a
// convention.
func (p RecoveryProof) Valid(current *model.TriageResult) bool {
	return p.AssessmentHash != "" &&
		p.AssessmentHash == hashAssessment(current) &&
		p.Authority != "" &&
		len(p.Disposable) > 0 &&
		p.EscrowVerified
}

// AuthorizesClear is the execution-time guard, checked IMMEDIATELY before each
// datadir clear. It makes clearing the authority — the catastrophic failure
// mode — impossible at the boundary, independent of any upstream triage
// conclusion: the instance must be in the disposable set AND must not be the
// authority.
func (p RecoveryProof) AuthorizesClear(instance string) bool {
	if instance == "" || instance == p.Authority {
		return false
	}
	for _, d := range p.Disposable {
		if d == instance {
			return true
		}
	}
	return false
}

// hashAssessment is a deterministic digest of the triage state that drives the
// recovery decision: the recovery projection (blocked/reason/authority/disposable/
// recoverySet), the authority status, and each instance's identity-and-position
// (classification, timeline, LSN, primary flag). Any drift in those between the
// proof's creation and the destructive edge changes the hash and fails Valid.
func hashAssessment(t *model.TriageResult) string {
	if t == nil {
		return ""
	}
	h := sha256.New()
	if r := t.Recovery; r != nil {
		fmt.Fprintf(h, "recovery|blocked=%t|reason=%s|authority=%s\n", r.Blocked, r.Reason, r.Authority)
		writeSortedLines(h, "disposable", r.Disposable)
		writeSortedLines(h, "recoverySet", r.RecoverySet)
	} else {
		fmt.Fprint(h, "recovery|none\n")
	}
	fmt.Fprintf(h, "authorityStatus=%s\n", t.AuthorityStatus)

	insts := append([]model.InstanceAssessment(nil), t.Assessments...)
	sort.Slice(insts, func(i, j int) bool { return insts[i].Pod < insts[j].Pod })
	for _, a := range insts {
		fmt.Fprintf(h, "inst|%s|class=%s|tl=%d|lsn=%s|primary=%t\n",
			a.Pod, a.Classification, a.Timeline, a.LSN, a.IsPrimary)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeSortedLines writes label|value lines in sorted order so the digest is
// independent of slice ordering.
func writeSortedLines(h io.Writer, label string, vals []string) {
	s := append([]string(nil), vals...)
	sort.Strings(s)
	for _, v := range s {
		fmt.Fprintf(h, "%s|%s\n", label, v)
	}
}
