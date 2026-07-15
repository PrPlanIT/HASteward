package repair

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
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
			model.InstanceAssessment{Pod: "c-2", Instance: 2, IsRunning: false, IsReady: false}, // skipped
		)
		if err := runEscrow(ctx, escrowCfg(), b, r, "c-0", "dump.sql"); err != nil {
			t.Fatal(err)
		}
		if len(b.calls) != 3 ||
			b.calls[0] != "backup:c-0:ns/c/dump.sql" ||
			b.calls[1] != "diverged:c-0:ns/c/0-dump.sql" ||
			b.calls[2] != "diverged:c-1:ns/c/1-dump.sql" {
			t.Fatalf("want [backup c-0, diverged c-0, diverged c-1] (c-2 skipped), got %v", b.calls)
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
