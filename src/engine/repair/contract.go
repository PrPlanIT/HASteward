package repair

import (
	"context"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// Repairer is the engine-specific hook contract for repair operations.
type Repairer interface {
	Name() string
	// DryRun reports whether this is a preview run. The service stops a dry-run after
	// Assess (the only read-only phase) — before SafetyGate, escrow, or heal touch the
	// cluster. A --dry-run must never mutate.
	DryRun() bool
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
	// VerifyRecovery is the loud-failure gate run after Stabilize + Reassess. It
	// confirms each healed instance recovered at the ENGINE's replication level —
	// not merely that its pod is Ready. CNPG marks an instance Ready while it is in
	// recovery, streaming or NOT, so "N/N Ready" can hide a standby whose walreceiver
	// never connected (a stale primary_conninfo in postgresql.auto.conf shadowing
	// CNPG's override.conf); Galera can be Ready but not Synced. Returns an error
	// naming any instance that is Ready-but-not-replicating, so the service reports a
	// FAILED/incomplete repair instead of a false green over a still-degraded cluster.
	// healed is the list of instances Heal touched (empty => no-op).
	VerifyRecovery(ctx context.Context, healed []string) error
	// Cleanup restores any cluster state the run mutated for its own duration
	// (e.g. a suspended operator CR) and MUST be safe to call on every exit path,
	// including partial failures and no-op runs. Invoked via defer by the service.
	Cleanup(ctx context.Context)
}

// HealTarget identifies a single instance to heal.
type HealTarget struct {
	Pod         string
	InstanceNum int
	Reason      string
	// Remediation is the action this target calls for:
	//   "restart" — non-destructive: delete the pod, CNPG recreates it against
	//               the same PVC and it streams back (data intact, WAL retained).
	//   "reseed" / "" — the destructive fence → clear → basebackup → unfence.
	Remediation string
}
