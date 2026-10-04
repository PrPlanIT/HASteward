package repair

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// recoverySetEscrow is a selected escrow provider plus the proven space headroom for
// capturing a recovery set. It exists so the three operations that must make a set of
// instances reversible before touching them — the deadlock breaker, a promotion, and a
// bare --escrow-only — share ONE orchestration instead of three copies of it. The copies
// had already drifted: only two of them checked free space, and each worded its refusals
// differently for the same failure.
type recoverySetEscrow struct {
	provider  escrow.EscrowProvider
	set       []string
	label     string // operation name, used as the refusal prefix ("unwedge", "promote", ...)
	estimate  int64
	available int64
}

// prepareRecoverySetEscrow selects the provider FAIL-CLOSED and proves there is room
// for the capture BEFORE anything is written — "requires X, only Y available", rather
// than discovering a full repository halfway through an escrow. It captures nothing, so
// a caller may report the plan and stop (--dry-run) without having mutated anything.
//
// It deliberately routes through selectEscrow rather than escrow.Select directly: that
// package variable is the seam the fail-closed contract is tested against, and the two
// callers that bypassed it had no test coverage of their refusals at all.
func prepareRecoverySetEscrow(ctx context.Context, cfg *common.Config, label string,
	t *model.TriageResult, set []string) (*recoverySetEscrow, error) {

	if len(set) == 0 {
		return nil, fmt.Errorf("%s REFUSED: empty recovery set — nothing to make reversible", label)
	}
	prov, err := selectEscrow(ctx, cfg, set)
	if err != nil {
		return nil, fmt.Errorf("%s REFUSED: %w", label, err)
	}
	est := prov.EstimateCaptureBytes(set, usedBytesByPVC(t, set))
	avail, err := prov.AvailableBytes()
	if err != nil {
		return nil, fmt.Errorf("%s REFUSED: cannot determine escrow free space: %w", label, err)
	}
	if est+breakerReserveBytes > avail {
		return nil, fmt.Errorf("%s REFUSED: escrow (%s) requires %s + %s reserve, only %s available in the escrow store",
			label, prov.Name(), output.FormatBytes(est), output.FormatBytes(breakerReserveBytes), output.FormatBytes(avail))
	}
	return &recoverySetEscrow{provider: prov, set: set, label: label, estimate: est, available: avail}, nil
}

// describe renders the chosen provider and the space it needs, for both the dry-run
// preview and the live run — so what a preview promises and what a run does are the
// same line of text.
func (e *recoverySetEscrow) describe() string {
	return fmt.Sprintf("%s — ~%s needed, %s available",
		e.provider.Name(), output.FormatBytes(e.estimate), output.FormatBytes(e.available))
}

// capture captures the recovery set and PROVES it restorable. Fail-closed at both
// stages: an unproven escrow is indistinguishable from none, so either failure must
// abort the caller rather than be logged.
func (e *recoverySetEscrow) capture(ctx context.Context) ([]escrow.EscrowRef, error) {
	refs, err := e.provider.Capture(ctx, e.set)
	if err != nil {
		return nil, fmt.Errorf("%s REFUSED: escrow capture failed: %w", e.label, err)
	}
	if err := e.provider.Verify(ctx, refs); err != nil {
		return nil, fmt.Errorf("%s REFUSED: escrow verification failed (rollback unproven): %w", e.label, err)
	}
	return refs, nil
}

// allInstances is every instance triage saw, which is the recovery set for an operation
// that has not chosen a survivor: before the choice is made, no lineage is disposable.
func allInstances(t *model.TriageResult) []string {
	out := make([]string, 0, len(t.Assessments))
	for _, a := range t.Assessments {
		out = append(out, a.Pod)
	}
	return out
}
