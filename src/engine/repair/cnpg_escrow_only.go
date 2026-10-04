package repair

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// errEscrowCaptured is returned by escrowOnly to STOP the repair pipeline cleanly once
// every instance is escrowed and proven. The run is complete and successful at that
// point; falling through to Assess/Heal would be the mutation the escrow was taken to
// make safe, which the operator has not asked for.
var errEscrowCaptured = errors.New("escrow-only: every instance escrowed and proven; nothing was mutated")

// escrowOnly is `repair --escrow-only`: it escrows and verifies EVERY instance, persists
// a decision record, and stops.
//
// It exists because the escrow a split-brain demands could not be obtained without first
// making the decision it protects against. --promote requires --instance N naming the
// survivor, and on a diverged cluster also --force — so an operator told to "escrow every
// instance, then choose" had to choose first. On a cluster with a provable authority it
// refuses outright, so a healthy cluster could not be escrowed this way at all.
//
// So this names no authority and no disposable set: before the choice is made, no lineage
// is disposable, and the recovery set is simply every instance. That is also why the
// proof it persists can never authorize a clear — RecoveryProof.Valid requires a known
// authority and a non-empty disposable set, both of which are deliberately absent here.
// The record exists to answer "what was captured, and from which cluster state", not to
// license a later mutation.
//
// Unlike a bare backup it runs inside the repair pipeline, so it holds the cluster-scoped
// OperationLock: nothing else HASteward can do will mutate the cluster underneath a
// capture. That is the whole reason it lives here rather than under the backup noun.
func (r *cnpgRepair) escrowOnly(ctx context.Context) (*model.TriageResult, error) {
	cfg := r.p.Config()
	if cfg.NoEscrow {
		return nil, fmt.Errorf("escrow-only REFUSED: --no-escrow cannot be combined with --escrow-only — capturing the escrow IS the operation")
	}

	// Raw triage, not Assess: Assess requires a running primary, and a cluster worth
	// escrowing frequently does not have one.
	t, err := triage.Run(ctx, r.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("escrow-only: triage failed: %w", err)
	}
	set := allInstances(t)

	output.Section("Escrow only (--escrow-only)")
	output.Field("Recovery set", strings.Join(set, ", "))
	if t.AuthorityStatus != "" {
		output.Field("Authority status", t.AuthorityStatus)
	}

	esc, err := prepareRecoverySetEscrow(ctx, cfg, "escrow-only", t, set)
	if err != nil {
		return nil, err
	}
	output.Field("Escrow", esc.describe())

	if cfg.DryRun {
		output.Plan("DRY RUN: would escrow %v via %s and stop — no instance touched, no survivor named",
			set, esc.provider.Name())
		return t, errDryRunPreview
	}

	refs, err := esc.capture(ctx)
	if err != nil {
		return nil, err
	}

	// Bind the record to the triage snapshot the capture was taken from, so a later
	// operator can tell whether the cluster has drifted since — the same binding the
	// breaker gates on, recorded here for the audit rather than for a gate.
	proof := RecoveryProof{
		AssessmentHash: hashAssessment(t),
		EscrowRefs:     refs,
		EscrowVerified: true,
	}
	if err := r.persistRecoveryProof(ctx, "repair-escrow-only", proof); err != nil {
		// The escrow itself succeeded and is RETAINED; only the record failed. Say so
		// precisely rather than implying the capture is gone.
		return nil, fmt.Errorf("escrow-only: escrow captured + verified (%v) but the decision record could not be persisted: %w",
			escrowIDs(refs), err)
	}

	output.Success("Escrow captured + verified for %d instance(s) — every lineage is reversible, nothing was mutated", len(refs))
	for _, ref := range refs {
		output.Bullet(0, "%s — %s %s", ref.Instance, ref.Provider, ref.ID)
	}
	return t, errEscrowCaptured
}
