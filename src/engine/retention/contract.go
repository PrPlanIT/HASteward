package retention

import (
	"context"
	"time"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// Retainer is the engine-specific hook contract for backup retention operations.
type Retainer interface {
	Name() string
	// Prune applies the retention policy and removes old snapshots.
	Prune(ctx context.Context, opts PruneOptions) (*model.PruneResult, error)
}

// PruneOptions holds the configuration for a prune operation.
type PruneOptions struct {
	Type        string // "backup", "diverged", "escrow", or "all"
	KeepLast    int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int

	// EscrowMinAge is the floor under escrow retention: an escrow younger than
	// this is kept no matter what the count policy says. It exists because an
	// escrow is the rollback for a change that has already been made, and the
	// window in which an operator discovers the change was wrong is exactly the
	// window in which the rollback must still exist.
	EscrowMinAge time.Duration
}
