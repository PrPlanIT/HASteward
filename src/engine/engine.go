package engine

import (
	"context"
	"fmt"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"
)

// Engine defines the interface that each database engine must implement.
type Engine interface {
	// Name returns the engine identifier ("cnpg" or "galera").
	Name() string

	// Validate verifies the target CR exists and sets engine-specific state.
	Validate(ctx context.Context, cfg *common.Config) error

	// Triage runs read-only diagnostics and returns the assessment.
	Triage(ctx context.Context) (*model.TriageResult, error)

	// Repair runs the full repair flow (safety gates, escrow, heal, re-triage).
	Repair(ctx context.Context) (*model.RepairResult, error)

	// Backup takes a backup of the cluster.
	Backup(ctx context.Context) (*model.BackupResult, error)

	// Restore restores the cluster from a backup.
	Restore(ctx context.Context) (*model.RestoreResult, error)

	// PruneWAL clears accumulated WAL from a disk-full instance.
	// This is a destructive operation — only safe when replicas are caught up.
	PruneWAL(ctx context.Context) error

	// Bootstrap performs a full cluster bootstrap when all nodes are down.
	// This is a DANGEROUS operation. It sets safe_to_bootstrap on the best candidate,
	// patches the CR, and brings the cluster back up.
	// Returns a BootstrapResult with decision and actions.
	Bootstrap(ctx context.Context, dryRun bool) (*model.BootstrapResult, error)
}

// registry maps engine names to constructor functions.
var registry = map[string]func() Engine{}

// Register adds an engine constructor to the registry.
func Register(name string, constructor func() Engine) {
	registry[name] = constructor
}

// Get returns a new engine instance by name, or an error if not found.
func Get(name string) (Engine, error) {
	constructor, ok := registry[name]
	if !ok {
		valid := make([]string, 0, len(registry))
		for k := range registry {
			valid = append(valid, k)
		}
		return nil, fmt.Errorf("unknown engine %q (valid: %v)", name, valid)
	}
	return constructor(), nil
}

// ValidEngines returns the list of registered engine names.
func ValidEngines() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	return names
}
