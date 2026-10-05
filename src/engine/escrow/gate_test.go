package escrow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// TestMain stubs the escrow census for the whole package: these tests exercise the gate's
// decisions, not the Kubernetes API, and the real implementation lists VolumeSnapshots
// against a live cluster. The cap's own behaviour is asserted explicitly in
// TestOutstandingEscrowCap, which overrides this.
func TestMain(m *testing.M) {
	CountOutstanding = func(context.Context, *common.Config) (int, error) { return 0, nil }
	os.Exit(m.Run())
}

// sizedProvider has controllable space accounting so the pre-capture refusal can be
// asserted, and records whether Capture ran — which is the point of that refusal: a full
// store must be caught BEFORE anything is written.
type sizedProvider struct {
	estimate   int64
	available  int64
	availErr   error
	failVerify bool
	captured   bool
}

func (s *sizedProvider) Name() string { return "sized" }
func (s *sizedProvider) Capture(ctx context.Context, set []string) ([]EscrowRef, error) {
	s.captured = true
	refs := make([]EscrowRef, 0, len(set))
	for _, p := range set {
		refs = append(refs, EscrowRef{Provider: s.Name(), ID: "id-" + p, PVC: p, Instance: p})
	}
	return refs, nil
}
func (s *sizedProvider) Verify(ctx context.Context, refs []EscrowRef) error {
	if s.failVerify {
		return fmt.Errorf("injected verify failure")
	}
	for i := range refs {
		refs[i].Verified = true
	}
	return nil
}
func (s *sizedProvider) Cleanup(ctx context.Context, refs []EscrowRef) error { return nil }
func (s *sizedProvider) EstimateCaptureBytes(set []string, used map[string]int64) int64 {
	return s.estimate
}
func (s *sizedProvider) AvailableBytes() (int64, error) { return s.available, s.availErr }

// withProvider swaps the fail-closed selector for one test.
func withProvider(t *testing.T, fn func(context.Context, *common.Config, string, []string) (EscrowProvider, error)) {
	t.Helper()
	prev := SelectProvider
	SelectProvider = fn
	t.Cleanup(func() { SelectProvider = prev })
}

func testCfg() *common.Config {
	return &common.Config{Namespace: "ns", ClusterName: "c", Engine: "cnpg", BackupsPath: "/b", ResticPassword: "pw"}
}

// Prepare is the single orchestration behind every destructive path. Before it existed
// each carried its own copy, two skipped the free-space check, and none of the refusals
// were covered.
func TestPrepare(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg()

	t.Run("empty recovery set is refused", func(t *testing.T) {
		if _, err := Prepare(ctx, cfg, "restore", nil, nil); err == nil {
			t.Fatal("an empty recovery set makes nothing reversible and must refuse")
		}
	})

	t.Run("a provider refusal carries the operation name", func(t *testing.T) {
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			return nil, fmt.Errorf("no provider can prove reversibility")
		})
		_, err := Prepare(ctx, cfg, "bootstrap", []string{"c-0"}, nil)
		if err == nil || !strings.HasPrefix(err.Error(), "bootstrap REFUSED") {
			t.Fatalf("a refusal must name the operation refused, got %v", err)
		}
	})

	t.Run("insufficient space refuses before anything is captured", func(t *testing.T) {
		p := &sizedProvider{estimate: 10 << 30, available: 1 << 30}
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) { return p, nil })
		if _, err := Prepare(ctx, cfg, "promote", []string{"c-0"}, nil); err == nil {
			t.Fatal("an escrow that cannot fit must be refused")
		}
		if p.captured {
			t.Fatal("the space check must run BEFORE Capture, not after")
		}
	})

	t.Run("the reserve is enforced, not just the estimate", func(t *testing.T) {
		// Fits the estimate exactly, leaving no reserve headroom.
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			return &sizedProvider{estimate: reserveBytes, available: reserveBytes}, nil
		})
		if _, err := Prepare(ctx, cfg, "unwedge", []string{"c-0"}, nil); err == nil {
			t.Fatal("an escrow must not be allowed to fill the store to the brim")
		}
	})

	t.Run("unknown free space is a refusal, not an assumption", func(t *testing.T) {
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			return &sizedProvider{availErr: fmt.Errorf("backend unreachable")}, nil
		})
		if _, err := Prepare(ctx, cfg, "restore", []string{"c-0"}, nil); err == nil {
			t.Fatal("space that cannot be determined must refuse")
		}
	})

	t.Run("an unproven capture is refused", func(t *testing.T) {
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			return &sizedProvider{available: 1 << 40, failVerify: true}, nil
		})
		rs, err := Prepare(ctx, cfg, "restore", []string{"c-0"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rs.Capture(ctx); err == nil {
			t.Fatal("an escrow that cannot be proven restorable is indistinguishable from none")
		}
	})
}

// Gate is the promise the whole tool rests on: a destructive operation takes a proven
// rollback, or it does not run.
func TestGate(t *testing.T) {
	ctx := context.Background()

	t.Run("captures and proves, reporting every ref", func(t *testing.T) {
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			return &sizedProvider{available: 1 << 40}, nil
		})
		refs, err := Gate(ctx, testCfg(), "restore", []string{"c-0", "c-1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 2 {
			t.Fatalf("every PVC in the set must be captured, got %d", len(refs))
		}
		for _, r := range refs {
			if !r.Verified {
				t.Fatalf("Verify must mark the refs it proved: %+v", r)
			}
		}
	})

	t.Run("a refusal aborts the caller", func(t *testing.T) {
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			return nil, fmt.Errorf("no provider")
		})
		if _, err := Gate(ctx, testCfg(), "bootstrap", []string{"c-0"}, nil); err == nil {
			t.Fatal("could-not-escrow must never become proceeded-anyway")
		}
	})

	// The one way past the gate is the operator saying so.
	t.Run("--no-escrow proceeds with no capture", func(t *testing.T) {
		withProvider(t, func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
			t.Fatal("--no-escrow must not select a provider at all")
			return nil, nil
		})
		cfg := testCfg()
		cfg.NoEscrow = true
		refs, err := Gate(ctx, cfg, "restore", []string{"c-0"}, nil)
		if err != nil || refs != nil {
			t.Fatalf("--no-escrow is an explicit opt-out, not a failure: refs=%v err=%v", refs, err)
		}
	})
}

type idNamer struct{}

func (idNamer) DataPVCName(pod string) string { return pod }

type storageNamer struct{}

func (storageNamer) DataPVCName(pod string) string { return "storage-" + pod }

// Galera was previously skipped for escrow on the premise that its PVC names were not
// derivable. The mapping was on EngineProvider all along.
func TestPVCsFor(t *testing.T) {
	if got := PVCsFor(idNamer{}, []string{"pg-0", "pg-1"}); got[0] != "pg-0" || got[1] != "pg-1" {
		t.Fatalf("cnpg maps a pod to itself: %v", got)
	}
	if got := PVCsFor(storageNamer{}, []string{"db-0"}); len(got) != 1 || got[0] != "storage-db-0" {
		t.Fatalf("galera maps a pod to storage-<pod>: %v", got)
	}
	if got := PVCsFor(idNamer{}, nil); len(got) != 0 {
		t.Fatalf("no pods means no PVCs: %v", got)
	}
}

func TestUsedBytesByPVC(t *testing.T) {
	tr := &model.TriageResult{Assessments: []model.InstanceAssessment{
		{Pod: "c-0", Disk: &model.DiskStats{UsedBytes: 100}},
		{Pod: "c-1"}, // no disk stats — must be absent, never fabricated as 0
	}}
	got := UsedBytesByPVC(tr, []string{"c-0", "c-1", "c-2"})
	if got["c-0"] != 100 {
		t.Fatalf("c-0 should report its used bytes: %v", got)
	}
	if _, ok := got["c-1"]; ok {
		t.Fatalf("an instance with no disk stats must be absent, not zero: %v", got)
	}
	if len(UsedBytesByPVC(nil, []string{"c-0"})) != 0 {
		t.Fatal("no triage means no sizes")
	}
}

// withOutstanding stubs the escrow census for one test.
func withOutstanding(t *testing.T, n int, err error) {
	t.Helper()
	prev := CountOutstanding
	CountOutstanding = func(context.Context, *common.Config) (int, error) { return n, err }
	t.Cleanup(func() { CountOutstanding = prev })
}

// The cap is what actually bounds a copy-on-write provider: its AvailableBytes has no
// honest number to report, so without this an escrow store could only ever grow.
func TestOutstandingEscrowCap(t *testing.T) {
	ctx := context.Background()
	ok := func(context.Context, *common.Config, string, []string) (EscrowProvider, error) {
		return &sizedProvider{available: 1 << 40}, nil
	}

	t.Run("under the cap proceeds", func(t *testing.T) {
		withProvider(t, ok)
		withOutstanding(t, maxOutstandingEscrows-1, nil)
		if _, err := Prepare(ctx, testCfg(), "restore", []string{"c-0"}, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("at the cap refuses and names the prune", func(t *testing.T) {
		withProvider(t, ok)
		withOutstanding(t, maxOutstandingEscrows, nil)
		_, err := Prepare(ctx, testCfg(), "restore", []string{"c-0"}, nil)
		if err == nil {
			t.Fatal("an escrow store that only grows protects nothing; this must refuse")
		}
		if !strings.Contains(err.Error(), "prune -t escrow") {
			t.Fatalf("the refusal must tell the operator how to clear it, got: %v", err)
		}
	})

	// A number you could not read is not evidence of headroom.
	t.Run("an unreadable census refuses", func(t *testing.T) {
		withProvider(t, ok)
		withOutstanding(t, 0, fmt.Errorf("api unreachable"))
		if _, err := Prepare(ctx, testCfg(), "restore", []string{"c-0"}, nil); err == nil {
			t.Fatal("failing to count must refuse, not assume zero")
		}
	})
}

// The kind is what retention reports, so the two long-standing values must survive the
// move to a shared gate rather than every escrow becoming "split-brain".
func TestEscrowKind(t *testing.T) {
	for op, want := range map[string]string{
		"escrow":            KindSplitBrain,
		"deadlock-recover":  KindDeadlock,
		"restore":           "restore",
		"bootstrap":         "bootstrap",
		"reset-authority":   "reset-authority",
	} {
		if got := escrowKind(op); got != want {
			t.Fatalf("escrowKind(%q) = %q, want %q", op, got, want)
		}
	}
}
