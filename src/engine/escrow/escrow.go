// Package escrow provides storage-agnostic, verified, reversible escrow of a
// recovery set (a set of PVCs) before a destructive in-place operation.
//
// The deadlock-breaker depends on EscrowProvider.Verify proving restorability —
// not on any particular backend — so the repair workflow sees only this interface
// and the Verified gate, never VolumeSnapshot vs. restic. This is deliberate: the
// safety invariant is verified reversibility, and providers satisfy it however
// their storage allows. It complements (does not replace) the live pg_dumpall
// escrow used by normal repair, which cannot run against a down instance.
package escrow

import (
	"context"
	"time"
)

// EscrowRef is a discoverable handle to one captured escrow of a single PVC.
// The cluster/instance/run-id/timestamp labels are deliberate: a human doing a
// manual restore months later must be able to find the escrow in seconds.
type EscrowRef struct {
	Provider  string    // "volumesnapshot" | "resticpvc"
	ID        string    // backend handle: VolumeSnapshot object name, or restic snapshot ID
	PVC       string    // the PVC this escrow protects
	Cluster   string    // owning database cluster
	Instance  string    // owning instance/pod
	RunID     string    // the operation run that created it
	CreatedAt time.Time // capture time (provider-stamped)
	Verified  bool      // Verify proved restorability — the only thing that may gate a destructive step
}

// EscrowProvider captures and verifies a reversible escrow of a recovery set.
// Implementations are storage-specific; the active provider is chosen fail-closed
// (see Select) — if no provider can prove reversibility, the breaker is unavailable.
type EscrowProvider interface {
	// Name is the provider identity recorded on every EscrowRef ("volumesnapshot"|"resticpvc").
	Name() string

	// Capture escrows every PVC in recoverySet, returning one EscrowRef per PVC.
	// It does not prove restorability — that is Verify's job — so a returned ref
	// is not yet safe to gate on (Verified is false until Verify succeeds).
	Capture(ctx context.Context, recoverySet []string) ([]EscrowRef, error)

	// Verify PROVES the refs are restorable. A nil return is the ONLY signal that
	// may gate a destructive operation; any non-nil error means do not proceed.
	// Implementations must prove recovery of actual bytes, not merely that a
	// snapshot object or repo entry exists.
	Verify(ctx context.Context, refs []EscrowRef) error

	// Cleanup releases escrow resources once the rollback window has closed
	// (e.g. the cluster is confirmed healthy). Best-effort; never gates safety.
	Cleanup(ctx context.Context, refs []EscrowRef) error
}
