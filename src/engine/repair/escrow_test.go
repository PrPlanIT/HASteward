package repair

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// fakeBacker records BackupDump calls as "type:donor:stdinFilename" and can be
// made to fail for a specific donor.
type fakeBacker struct {
	calls  []string
	failOn string
}

func (f *fakeBacker) Name() string                                            { return "fake" }
func (f *fakeBacker) Backup(ctx context.Context) (*model.BackupResult, error) { return nil, nil }
func (f *fakeBacker) BackupDump(ctx context.Context, backupType, donor, stdinFilename string, jobTime time.Time, extraTags map[string]string) (*model.BackupResult, error) {
	f.calls = append(f.calls, backupType+":"+donor+":"+stdinFilename)
	if donor == f.failOn {
		return nil, fmt.Errorf("injected backup failure for %s", donor)
	}
	return &model.BackupResult{SnapshotID: "snap-" + donor}, nil
}

// TestMain stubs the repository presence check for the whole package: these tests
// exercise escrow orchestration, not restic, and the default would shell out to a
// binary that is not present. The absent-repository refusal is asserted explicitly
// in TestRunEscrowRepositoryMustPreExist.
func TestMain(m *testing.M) {
	escrowRepoExists = func(context.Context, *common.Config) (bool, error) { return true, nil }
	os.Exit(m.Run())
}

func escrowCfg() *common.Config {
	return &common.Config{Namespace: "ns", ClusterName: "c", BackupsPath: "/b", ResticPassword: "pw"}
}
func triageRes(safe bool, a ...model.InstanceAssessment) *model.TriageResult {
	return &model.TriageResult{DataComparison: model.DataComparison{SafeToHeal: safe}, Assessments: a}
}

func TestRunEscrow(t *testing.T) {
	ctx := context.Background()

	t.Run("safe: one pre-repair backup, no diverged", func(t *testing.T) {
		b := &fakeBacker{}
		if err := runEscrow(ctx, escrowCfg(), b, triageRes(true), "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(b.calls) != 1 || b.calls[0] != "backup:c-0:ns/c/dump.sql" {
			t.Fatalf("calls = %v", b.calls)
		}
	})

	t.Run("split-brain: pre-repair + diverged per running node", func(t *testing.T) {
		b := &fakeBacker{}
		r := triageRes(false,
			model.InstanceAssessment{Pod: "c-0", Instance: 0, IsRunning: true, IsReady: true},
			model.InstanceAssessment{Pod: "c-1", Instance: 1, IsRunning: true, IsReady: true},
			// Not dumpable. Routed to block-level escrow, which is a no-op here because
			// this cfg names no engine — see TestRunEscrowDownDivergedInstance.
			model.InstanceAssessment{Pod: "c-2", Instance: 2, IsRunning: false, IsReady: false},
		)
		if err := runEscrow(ctx, escrowCfg(), b, r, "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(b.calls) != 3 ||
			b.calls[0] != "backup:c-0:ns/c/dump.sql" ||
			b.calls[1] != "diverged:c-0:ns/c/0-dump.sql" ||
			b.calls[2] != "diverged:c-1:ns/c/1-dump.sql" {
			t.Fatalf("want [backup c-0, diverged c-0, diverged c-1] (c-2 is not dumpable), got %v", b.calls)
		}
	})

	t.Run("no_escrow: no backups at all", func(t *testing.T) {
		b := &fakeBacker{}
		cfg := escrowCfg()
		cfg.NoEscrow = true
		r := triageRes(false, model.InstanceAssessment{Pod: "c-0", IsRunning: true, IsReady: true})
		if err := runEscrow(ctx, cfg, b, r, "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(b.calls) != 0 {
			t.Fatalf("no_escrow must skip all backups, got %v", b.calls)
		}
	})

	t.Run("missing creds: error", func(t *testing.T) {
		cfg := escrowCfg()
		cfg.ResticPassword = ""
		if err := runEscrow(ctx, cfg, &fakeBacker{}, triageRes(true), "c-0", "dump.sql"); err == nil {
			t.Fatal("expected an error when RESTIC_PASSWORD is unset")
		}
	})

	t.Run("empty donor: skips pre-repair, still runs diverged", func(t *testing.T) {
		b := &fakeBacker{}
		r := triageRes(false, model.InstanceAssessment{Pod: "c-0", Instance: 0, IsRunning: true, IsReady: true})
		if err := runEscrow(ctx, escrowCfg(), b, r, "", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(b.calls) != 1 || b.calls[0] != "diverged:c-0:ns/c/0-dump.sql" {
			t.Fatalf("want only the diverged backup, got %v", b.calls)
		}
	})

	t.Run("pre-repair backup fails: error", func(t *testing.T) {
		b := &fakeBacker{failOn: "c-0"}
		if err := runEscrow(ctx, escrowCfg(), b, triageRes(true), "c-0", "dump.sql"); err == nil {
			t.Fatal("expected an error when the pre-repair backup fails")
		}
	})

	t.Run("diverged failure is best-effort: no error", func(t *testing.T) {
		b := &fakeBacker{failOn: "c-0"} // c-0's diverged backup fails; donor c-9 succeeds
		r := triageRes(false,
			model.InstanceAssessment{Pod: "c-0", Instance: 0, IsRunning: true, IsReady: true},
			model.InstanceAssessment{Pod: "c-1", Instance: 1, IsRunning: true, IsReady: true},
		)
		if err := runEscrow(ctx, escrowCfg(), b, r, "c-9", "dump.sql"); err != nil {
			t.Fatalf("a diverged failure must not abort escrow: %v", err)
		}
	})
}

// fakeEscrowProvider records what was captured and can fail at either stage, so the
// "not best-effort" contract can be asserted without a storage backend.
type fakeEscrowProvider struct {
	captured    []string
	failCapture bool
	failVerify  bool
	verified    bool
}

func (f *fakeEscrowProvider) Name() string { return "fake-escrow" }
func (f *fakeEscrowProvider) Capture(ctx context.Context, set []string) ([]escrow.EscrowRef, error) {
	f.captured = append(f.captured, set...)
	if f.failCapture {
		return nil, fmt.Errorf("injected capture failure")
	}
	refs := make([]escrow.EscrowRef, 0, len(set))
	for _, p := range set {
		refs = append(refs, escrow.EscrowRef{Provider: "fake-escrow", ID: "id-" + p, PVC: p})
	}
	return refs, nil
}
func (f *fakeEscrowProvider) Verify(ctx context.Context, refs []escrow.EscrowRef) error {
	if f.failVerify {
		return fmt.Errorf("injected verify failure")
	}
	f.verified = true
	return nil
}
func (f *fakeEscrowProvider) Cleanup(ctx context.Context, refs []escrow.EscrowRef) error { return nil }
func (f *fakeEscrowProvider) EstimateCaptureBytes(set []string, used map[string]int64) int64 {
	return 0
}
func (f *fakeEscrowProvider) AvailableBytes() (int64, error) { return 1 << 40, nil }

// withSelectEscrow swaps the package's provider selector for one test.
func withSelectEscrow(t *testing.T, fn func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error)) {
	t.Helper()
	prev := selectEscrow
	selectEscrow = fn
	t.Cleanup(func() { selectEscrow = prev })
}

func cnpgEscrowCfg() *common.Config {
	c := escrowCfg()
	c.Engine = "cnpg"
	return c
}

// A down instance on a split-brain is frequently the divergent one, so it is the
// lineage nothing else holds a copy of. These assert it is captured rather than
// skipped, and that a failure to capture it stops the run instead of being logged.
func TestRunEscrowDownDivergedInstance(t *testing.T) {
	ctx := context.Background()

	// c-0 and c-1 are dumpable; c-2 is down and must be escrowed at the block layer.
	downSplitBrain := func() *model.TriageResult {
		return triageRes(false,
			model.InstanceAssessment{Pod: "c-0", Instance: 0, IsRunning: true, IsReady: true},
			model.InstanceAssessment{Pod: "c-1", Instance: 1, IsRunning: true, IsReady: true},
			model.InstanceAssessment{Pod: "c-2", Instance: 2, IsRunning: false, IsReady: false},
		)
	}

	t.Run("down instance is captured, not skipped", func(t *testing.T) {
		p := &fakeEscrowProvider{}
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return p, nil
		})
		b := &fakeBacker{}
		if err := runEscrow(ctx, cnpgEscrowCfg(), b, downSplitBrain(), "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(p.captured) != 1 || p.captured[0] != "c-2" {
			t.Fatalf("want the down instance c-2 escrowed, captured = %v", p.captured)
		}
		if !p.verified {
			t.Fatal("capture must be verified — an unproven escrow is indistinguishable from none")
		}
		// The running instances still go through the dump path, unchanged.
		if len(b.calls) != 3 || b.calls[2] != "diverged:c-1:ns/c/1-dump.sql" {
			t.Fatalf("dump path changed: %v", b.calls)
		}
	})

	t.Run("no provider: refusal aborts the run", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return nil, fmt.Errorf("no provider can prove reversibility")
		})
		if err := runEscrow(ctx, cnpgEscrowCfg(), &fakeBacker{}, downSplitBrain(), "c-0", "dump.sql"); err == nil {
			t.Fatal("a down diverged instance that cannot be escrowed must abort, not warn")
		}
	})

	t.Run("capture failure aborts the run", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return &fakeEscrowProvider{failCapture: true}, nil
		})
		if err := runEscrow(ctx, cnpgEscrowCfg(), &fakeBacker{}, downSplitBrain(), "c-0", "dump.sql"); err == nil {
			t.Fatal("a failed capture of the only copy of a lineage must abort")
		}
	})

	t.Run("unverified capture aborts the run", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return &fakeEscrowProvider{failVerify: true}, nil
		})
		if err := runEscrow(ctx, cnpgEscrowCfg(), &fakeBacker{}, downSplitBrain(), "c-0", "dump.sql"); err == nil {
			t.Fatal("a capture that cannot be proven restorable must abort")
		}
	})

	t.Run("all instances up: no block-level escrow attempted", func(t *testing.T) {
		p := &fakeEscrowProvider{}
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return p, nil
		})
		r := triageRes(false, model.InstanceAssessment{Pod: "c-0", Instance: 0, IsRunning: true, IsReady: true})
		if err := runEscrow(ctx, cnpgEscrowCfg(), &fakeBacker{}, r, "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(p.captured) != 0 {
			t.Fatalf("nothing was undumpable, captured = %v", p.captured)
		}
	})

	t.Run("galera: gap is reported, run continues", func(t *testing.T) {
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			t.Fatal("galera PVC names are not derivable — selection must not be attempted")
			return nil, nil
		})
		cfg := escrowCfg()
		cfg.Engine = "galera"
		if err := runEscrow(ctx, cfg, &fakeBacker{}, downSplitBrain(), "c-0", "dump.sql"); err != nil {
			t.Fatalf("galera must warn rather than abort: %v", err)
		}
	})

	t.Run("safe cluster: no diverged escrow at all", func(t *testing.T) {
		p := &fakeEscrowProvider{}
		withSelectEscrow(t, func(context.Context, *common.Config, []string) (escrow.EscrowProvider, error) {
			return p, nil
		})
		r := triageRes(true, model.InstanceAssessment{Pod: "c-2", Instance: 2, IsRunning: false, IsReady: false})
		if err := runEscrow(ctx, cnpgEscrowCfg(), &fakeBacker{}, r, "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(p.captured) != 0 {
			t.Fatalf("a down instance on a HEALTHY cluster is not a diverged lineage, captured = %v", p.captured)
		}
	})
}

// withEscrowRepo swaps the repository presence check for one test.
func withEscrowRepo(t *testing.T, exists bool, err error) {
	t.Helper()
	prev := escrowRepoExists
	escrowRepoExists = func(context.Context, *common.Config) (bool, error) { return exists, err }
	t.Cleanup(func() { escrowRepoExists = prev })
}

// BackupDump initialises a restic repository on demand. That is right for a first
// `backup create` and wrong for an escrow: a repository created by this run holds
// nothing, and under the container wrapper (kubeconfig is the only mount) it is a
// directory inside the container that is discarded on exit — so the escrow would
// report a snapshot ID, satisfy the safety gate, and not exist.
func TestRunEscrowRepositoryMustPreExist(t *testing.T) {
	ctx := context.Background()

	t.Run("absent repository: refuse before any backup runs", func(t *testing.T) {
		withEscrowRepo(t, false, nil)
		b := &fakeBacker{}
		err := runEscrow(ctx, escrowCfg(), b, triageRes(true), "c-0", "dump.sql")
		if err == nil {
			t.Fatal("an escrow repository that this run would create proves nothing — must refuse")
		}
		if len(b.calls) != 0 {
			t.Fatalf("refusal must come before any dump, got %v", b.calls)
		}
	})

	t.Run("unreachable backend: refuse, distinct from absent", func(t *testing.T) {
		withEscrowRepo(t, false, fmt.Errorf("dial tcp: connection refused"))
		if err := runEscrow(ctx, escrowCfg(), &fakeBacker{}, triageRes(true), "c-0", "dump.sql"); err == nil {
			t.Fatal("an unreachable escrow backend must refuse, not proceed")
		}
	})

	t.Run("present repository: proceeds", func(t *testing.T) {
		withEscrowRepo(t, true, nil)
		b := &fakeBacker{}
		if err := runEscrow(ctx, escrowCfg(), b, triageRes(true), "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(b.calls) != 1 {
			t.Fatalf("calls = %v", b.calls)
		}
	})

	t.Run("no_escrow: presence is not required", func(t *testing.T) {
		withEscrowRepo(t, false, nil)
		cfg := escrowCfg()
		cfg.NoEscrow = true
		if err := runEscrow(ctx, cfg, &fakeBacker{}, triageRes(true), "c-0", "dump.sql"); err != nil {
			t.Fatalf("--no-escrow opts out of escrow entirely: %v", err)
		}
	})
}

// Each diverged lineage must land on its own path. Instance is what distinguishes
// them, and a live CNPG triage reported 0 for every instance — which sent all three
// of gitlab-postgresql's lineages to ns/cluster/0-dump.sql, leaving the snapshots
// indistinguishable at exactly the moment one has to be chosen.
func TestRunEscrowDivergedFilenamesAreDistinct(t *testing.T) {
	b := &fakeBacker{}
	r := triageRes(false,
		model.InstanceAssessment{Pod: "c-1", Instance: 1, IsRunning: true, IsReady: true},
		model.InstanceAssessment{Pod: "c-2", Instance: 2, IsRunning: true, IsReady: true},
		model.InstanceAssessment{Pod: "c-3", Instance: 3, IsRunning: true, IsReady: true},
	)
	if err := runEscrow(context.Background(), escrowCfg(), b, r, "c-1", "dump.sql"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range b.calls {
		if seen[c] {
			t.Fatalf("two lineages escrowed to the same path: %q (calls = %v)", c, b.calls)
		}
		seen[c] = true
	}
	for _, want := range []string{"diverged:c-1:ns/c/1-dump.sql", "diverged:c-2:ns/c/2-dump.sql", "diverged:c-3:ns/c/3-dump.sql"} {
		if !seen[want] {
			t.Fatalf("missing %q in %v", want, b.calls)
		}
	}
}
