package repair

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// recordingRepairer implements Repairer and records which phases the service invokes,
// so we can prove a dry-run never reaches a mutating phase.
type recordingRepairer struct {
	dryRun    bool
	calls     []string
	verifyErr error // when set, VerifyRecovery returns it (simulates a Ready-but-not-replicating heal)
}

func (f *recordingRepairer) note(s string) { f.calls = append(f.calls, s) }
func (f *recordingRepairer) Name() string  { return "fake" }
func (f *recordingRepairer) DryRun() bool  { return f.dryRun }
func (f *recordingRepairer) OperationLock(ctx context.Context) (func(), error) {
	f.note("lock")
	return func() {}, nil
}
func (f *recordingRepairer) PreAssess(ctx context.Context) (*model.TriageResult, error) {
	f.note("preassess")
	return nil, nil
}
func (f *recordingRepairer) Assess(ctx context.Context) (*model.TriageResult, error) {
	f.note("assess")
	return &model.TriageResult{Cluster: model.ObjectRef{Name: "c", Namespace: "ns"}}, nil
}
func (f *recordingRepairer) SafetyGate(ctx context.Context, t *model.TriageResult) error {
	f.note("safetygate") // MUTATES: suspends the operator CR
	return nil
}
func (f *recordingRepairer) Escrow(ctx context.Context, t *model.TriageResult) error {
	f.note("escrow") // MUTATES: writes backups
	return nil
}
func (f *recordingRepairer) PlanTargets(ctx context.Context, t *model.TriageResult) ([]HealTarget, error) {
	f.note("plan")
	return []HealTarget{{Pod: "c-0"}}, nil
}
func (f *recordingRepairer) Heal(ctx context.Context, target HealTarget) error {
	f.note("heal") // MUTATES: clears datadir + rebuilds
	return nil
}
func (f *recordingRepairer) Stabilize(ctx context.Context) error { f.note("stabilize"); return nil }
func (f *recordingRepairer) Reassess(ctx context.Context) (*model.TriageResult, error) {
	f.note("reassess")
	return nil, nil
}
func (f *recordingRepairer) VerifyRecovery(ctx context.Context, healed []string) error {
	f.note("verify")
	return f.verifyErr
}
func (f *recordingRepairer) Cleanup(ctx context.Context) { f.note("cleanup") }

// TestRepairRun_DryRunStopsBeforeMutation guards #28: a --dry-run must stop after the
// read-only triage and never reach SafetyGate (suspends the CR), Escrow, or Heal.
func TestRepairRun_DryRunStopsBeforeMutation(t *testing.T) {
	f := &recordingRepairer{dryRun: true}
	if _, err := Run(context.Background(), f, engine.NopSink{}); err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	for _, mutating := range []string{"safetygate", "escrow", "plan", "heal", "stabilize"} {
		if slices.Contains(f.calls, mutating) {
			t.Fatalf("dry-run must NOT reach %q — it mutates the cluster (#28); calls=%v", mutating, f.calls)
		}
	}
	if !slices.Contains(f.calls, "assess") {
		t.Fatalf("dry-run should still run the read-only triage; calls=%v", f.calls)
	}
	if !slices.Contains(f.calls, "cleanup") {
		t.Fatalf("dry-run should still run the deferred cleanup backstop; calls=%v", f.calls)
	}
}

// TestRepairRun_RealRunReachesHeal confirms the gate is dry-run-specific: a real run
// proceeds through the mutating phases as before.
func TestRepairRun_RealRunReachesHeal(t *testing.T) {
	f := &recordingRepairer{dryRun: false}
	if _, err := Run(context.Background(), f, engine.NopSink{}); err != nil {
		t.Fatalf("real run should not error: %v", err)
	}
	for _, phase := range []string{"safetygate", "escrow", "plan", "heal"} {
		if !slices.Contains(f.calls, phase) {
			t.Fatalf("a real run must reach %q; calls=%v", phase, f.calls)
		}
	}
	// A real run must also reach the post-repair recovery verification.
	if !slices.Contains(f.calls, "verify") {
		t.Fatalf("a real run must reach %q (post-repair streaming/Synced gate); calls=%v", "verify", f.calls)
	}
}

// TestRepairRun_VerifyFailureFailsRun guards the "N/N Ready masks broken replication"
// trap: when the heal leaves an instance Ready-but-not-replicating, VerifyRecovery
// returns an error and the whole run must fail loud (non-nil error) rather than
// reporting a false green — even though every earlier phase "succeeded".
func TestRepairRun_VerifyFailureFailsRun(t *testing.T) {
	f := &recordingRepairer{dryRun: false, verifyErr: errFakeNotStreaming}
	_, err := Run(context.Background(), f, engine.NopSink{})
	if err == nil {
		t.Fatalf("a heal that verifies un-recovered must fail the run; got nil error; calls=%v", f.calls)
	}
	if !slices.Contains(f.calls, "verify") {
		t.Fatalf("verify phase should have run; calls=%v", f.calls)
	}
}

var errFakeNotStreaming = errors.New("instance c-0 Ready but NOT streaming from primary")
