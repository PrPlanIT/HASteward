package repair

import (
	"context"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// Repairer is the engine-specific hook contract for repair operations.
type Repairer interface {
	Name() string
	// PreAssess is repair Phase 0 — the deadlock breaker. Inert (returns nil)
	// unless enabled (--unwedge) and a breakable deadlock is detected.
	PreAssess(ctx context.Context) (*model.TriageResult, error)
	Assess(ctx context.Context) (*model.TriageResult, error)
	SafetyGate(ctx context.Context, triage *model.TriageResult) error
	Escrow(ctx context.Context, triage *model.TriageResult) error
	PlanTargets(ctx context.Context, triage *model.TriageResult) ([]HealTarget, error)
	Heal(ctx context.Context, target HealTarget) error
	Stabilize(ctx context.Context) error
	Reassess(ctx context.Context) (*model.TriageResult, error)
}

// HealTarget identifies a single instance to heal.
type HealTarget struct {
	Pod         string
	InstanceNum int
	Reason      string
}
