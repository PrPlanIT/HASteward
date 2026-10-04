package repair

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// sizedProvider is a provider with controllable space accounting, so the pre-capture
// space refusal can be asserted. It also records whether Capture ran, which is the
// point of that refusal: a full repository must be caught BEFORE anything is written.
type sizedProvider struct {
	estimate   int64
	available  int64
	availErr   error
	failVerify bool
	captured   bool
}

func (s *sizedProvider) Name() string { return "sized" }
func (s *sizedProvider) Capture(ctx context.Context, set []string) ([]escrow.EscrowRef, error) {
	s.captured = true
	refs := make([]escrow.EscrowRef, 0, len(set))
	for _, p := range set {
		refs = append(refs, escrow.EscrowRef{Provider: s.Name(), ID: "id-" + p, PVC: p, Instance: p})
	}
	return refs, nil
}
func (s *sizedProvider) Verify(ctx context.Context, refs []escrow.EscrowRef) error {
	if s.failVerify {
		return fmt.Errorf("injected verify failure")
	}
	return nil
}
func (s *sizedProvider) Cleanup(ctx context.Context, refs []escrow.EscrowRef) error { return nil }
func (s *sizedProvider) EstimateCaptureBytes(set []string, used map[string]int64) int64 {
	return s.estimate
}
func (s *sizedProvider) AvailableBytes() (int64, error) { return s.available, s.availErr }

func escrowTriage(pods ...string) *model.TriageResult {
	t := &model.TriageResult{}
	for i, p := range pods {
		t.Assessments = append(t.Assessments, model.InstanceAssessment{Pod: p, Instance: i})
	}
	return t
}

// prepareRecoverySetEscrow is the single orchestration behind --unwedge, --promote and
// the diverged-instance escrow in repair. Before it existed each carried its own copy, two
// bypassed the selectEscrow seam, and none of their refusals were covered.
func TestPrepareRecoverySetEscrow(t *testing.T) {
	ctx := context.Background()
	cfg := &common.Config{Namespace: "ns", ClusterName: "c", Engine: "cnpg"}
	tr := escrowTriage("c-0", "c-1")

	t.Run("empty recovery set is refused", func(t *testing.T) {
		if _, err := prepareRecoverySetEscrow(ctx, cfg, "escrow", tr, nil); err == nil {
			t.Fatal("an empty recovery set makes nothing reversible and must refuse")
		}
	})

	t.Run("a provider refusal carries the operation label", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return nil, fmt.Errorf("no provider can prove reversibility")
		})
		_, err := prepareRecoverySetEscrow(ctx, cfg, "unwedge", tr, []string{"c-0"})
		if err == nil || !strings.HasPrefix(err.Error(), "unwedge REFUSED") {
			t.Fatalf("refusal must name the operation that was refused, got %v", err)
		}
	})

	// The reason the space check precedes the capture: an operator must see "requires X,
	// only Y available" rather than discover a full repository mid-escrow.
	t.Run("insufficient space refuses before anything is captured", func(t *testing.T) {
		p := &sizedProvider{estimate: 10 << 30, available: 1 << 30}
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return p, nil
		})
		if _, err := prepareRecoverySetEscrow(ctx, cfg, "promote", tr, []string{"c-0"}); err == nil {
			t.Fatal("an escrow that cannot fit must be refused")
		}
		if p.captured {
			t.Fatal("the space check must run BEFORE Capture, not after")
		}
	})

	t.Run("the reserve is enforced, not just the estimate", func(t *testing.T) {
		// Fits the estimate exactly but leaves no reserve headroom.
		p := &sizedProvider{estimate: 1 << 30, available: 1 << 30}
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return p, nil
		})
		if _, err := prepareRecoverySetEscrow(ctx, cfg, "unwedge", tr, []string{"c-0"}); err == nil {
			t.Fatal("an escrow must not be allowed to fill the store to the brim")
		}
	})

	t.Run("unknown free space is a refusal, not an assumption", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return &sizedProvider{availErr: fmt.Errorf("backend unreachable")}, nil
		})
		if _, err := prepareRecoverySetEscrow(ctx, cfg, "escrow", tr, []string{"c-0"}); err == nil {
			t.Fatal("space that cannot be determined must refuse")
		}
	})

	t.Run("capture + verify succeed", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return &sizedProvider{available: 1 << 40}, nil
		})
		esc, err := prepareRecoverySetEscrow(ctx, cfg, "escrow", tr, []string{"c-0", "c-1"})
		if err != nil {
			t.Fatal(err)
		}
		refs, err := esc.capture(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 2 {
			t.Fatalf("every instance in the set must be captured, got %d", len(refs))
		}
	})

	t.Run("an unproven capture is refused", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return &sizedProvider{available: 1 << 40, failVerify: true}, nil
		})
		esc, err := prepareRecoverySetEscrow(ctx, cfg, "escrow", tr, []string{"c-0"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := esc.capture(ctx); err == nil {
			t.Fatal("an escrow that cannot be proven restorable is indistinguishable from none")
		}
	})
}

// allInstances is the recovery set for an operation that has named no survivor: until
// the choice is made, no lineage is disposable.
func TestAllInstances(t *testing.T) {
	got := allInstances(escrowTriage("c-2", "c-0", "c-1"))
	if len(got) != 3 || got[0] != "c-2" {
		t.Fatalf("every assessed instance must be in the set, in triage order: %v", got)
	}
	if len(allInstances(&model.TriageResult{})) != 0 {
		t.Fatal("no assessments means no recovery set")
	}
}
