package repair

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// Run is the shared repair lifecycle. All engines go through this flow.
func Run(ctx context.Context, r Repairer, sink engine.StepSink) (*model.RepairResult, error) {
	start := time.Now()
	result := &model.RepairResult{Engine: r.Name()}

	// Phase -1: Serialize against other HASteward mutations on this cluster. The whole
	// operation runs under one exclusive cluster lock — repair/unwedge/prune-WAL share the
	// cnpg.io/reconciliationLoop switch and the read-modify-write fencedInstances
	// annotation, so concurrent operations would corrupt each other's ownership window.
	release, err := r.OperationLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	// Phase 0: Deadlock breaker. Inert unless --unwedge and a breakable deadlock is
	// detected; when it fires it clears disposable datadirs offline (escrow-gated)
	// so the subsequent Assess finds a healthy primary instead of aborting.
	sink.Step("pre-assess", "running")
	if _, err := r.PreAssess(ctx); err != nil {
		// A dry-run preview is a clean stop, not a failure: do not fall through to
		// Assess (which aborts while the cluster is still frozen).
		if errors.Is(err, errDryRunPreview) {
			sink.Step("pre-assess", "done")
			result.Duration = time.Since(start)
			return result, nil
		}
		return nil, err
	}
	sink.Step("pre-assess", "done")

	// Phase 1: Assess
	sink.Step("assess", "running")
	triage, err := r.Assess(ctx)
	if err != nil {
		return nil, fmt.Errorf("triage failed: %w", err)
	}
	result.Cluster = triage.Cluster
	sink.Step("assess", "done")

	// Phase 2: Safety gate
	sink.Step("safety-gate", "running")
	if err := r.SafetyGate(ctx, triage); err != nil {
		return nil, err
	}
	sink.Step("safety-gate", "done")

	// Phase 3: Escrow
	sink.Step("escrow", "running")
	if err := r.Escrow(ctx, triage); err != nil {
		return nil, err
	}
	sink.Step("escrow", "done")

	// Phase 4: Plan targets
	sink.Step("plan", "running")
	targets, err := r.PlanTargets(ctx, triage)
	if err != nil {
		return nil, err
	}
	sink.Step("plan", "done")

	if len(targets) == 0 {
		result.Duration = time.Since(start)
		return result, nil
	}

	// Phase 5: Heal each target
	for _, t := range targets {
		sink.Step("heal-"+t.Pod, "running")
		if err := r.Heal(ctx, t); err != nil {
			return nil, fmt.Errorf("heal failed for %s: %w", t.Pod, err)
		}
		result.HealedInstances = append(result.HealedInstances, t.Pod)
		sink.Step("heal-"+t.Pod, "done")
	}

	// Phase 6: Stabilize + reassess
	sink.Step("stabilize", "running")
	r.Stabilize(ctx)
	sink.Step("stabilize", "done")

	sink.Step("reassess", "running")
	postTriage, _ := r.Reassess(ctx)
	result.PostTriageResult = postTriage
	sink.Step("reassess", "done")

	result.Duration = time.Since(start)
	return result, nil
}
