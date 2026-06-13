package repair

import (
	"context"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// Repairer is the engine-specific hook contract for repair operations.
type Repairer interface {
	Name() string
	// OperationLock acquires an exclusive, cluster-scoped lock for the whole repair
	// operation, serializing it against other HASteward mutations on the same cluster
	// (which share the cnpg.io/reconciliationLoop switch and fencedInstances annotation).
	// Returns a release func, invoked when the operation ends. Engines without shared
	// mutable cluster-scoped state return a no-op.
	OperationLock(ctx context.Context) (func(), error)
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
