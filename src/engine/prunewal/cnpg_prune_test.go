package prunewal

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// execRouter routes canned responses to the Go-driven prune's exec calls and records the
// rm it issues, so the prune logic is fully testable without a live cluster.
type execRouter struct {
	controlData string            // pg_controldata output
	replayLSN   map[string]string // replica pod -> pg_last_wal_replay_lsn() output
	segments    []string          // absolute WAL segment paths returned by `find`
	rmArgs      []string          // captured paths passed to `rm -f`
	rmCalled    bool
	findCalled  bool
}

func (r *execRouter) hook() func(ctx context.Context, pod, ns, container string, command []string) (*k8s.ExecResult, error) {
	return func(ctx context.Context, pod, ns, container string, command []string) (*k8s.ExecResult, error) {
		joined := strings.Join(command, " ")
		switch {
		case command[0] == "pg_controldata":
			return &k8s.ExecResult{Stdout: r.controlData}, nil
		case strings.Contains(joined, "pg_last_wal_replay_lsn"):
			return &k8s.ExecResult{Stdout: r.replayLSN[pod]}, nil
		case strings.Contains(joined, "-regex"):
			r.findCalled = true
			return &k8s.ExecResult{Stdout: strings.Join(r.segments, "\n")}, nil
		case command[0] == "rm":
			r.rmCalled = true
			r.rmArgs = command[2:] // drop "rm", "-f"
			return &k8s.ExecResult{}, nil
		default: // the .partial/.backup cleanup find
			return &k8s.ExecResult{}, nil
		}
	}
}

func testPruner(force bool) *cnpgPruner {
	p := provider.NewCNPGProviderForTest(
		&common.Config{ClusterName: "pg", Namespace: "ns", Force: force}, 3, nil)
	return &cnpgPruner{p: p}
}

const controlDataREDO5 = `pg_control version number:            1300
Latest checkpoint's REDO location:    0/5000028
Latest checkpoint's REDO WAL file:    000000010000000000000005
Latest checkpoint's TimeLineID:       1`

func walPath(seg string) string {
	return "/var/lib/postgresql/data/pgdata/pg_wal/" + seg
}

// TestPruneWALOnPVC_AbortsWhenReplicaLags is the #23 guard: a ready replica whose replay
// LSN is behind the checkpoint still needs pre-checkpoint WAL, so the prune must abort
// and delete NOTHING.
func TestPruneWALOnPVC_AbortsWhenReplicaLags(t *testing.T) {
	r := &execRouter{
		controlData: controlDataREDO5,
		replayLSN:   map[string]string{"pg-2": "0/4000000"}, // behind 0/5000028
	}
	defer k8s.SetExecHookForTest(r.hook())()

	err := testPruner(false).pruneWALOnPVC(context.Background(), "pg-prune", "ns", []string{"pg-2"}, true)
	if err == nil || !strings.Contains(err.Error(), "lag") {
		t.Fatalf("prune must abort when a replica lags behind the checkpoint (#23), got: %v", err)
	}
	if r.rmCalled || r.findCalled {
		t.Fatal("prune listed/deleted WAL despite a lagging replica — must abort BEFORE touching any segment (#23)")
	}
}

// TestPruneWALOnPVC_DeletesOnlyOlderSegmentsWhenCaughtUp verifies the delete decision is
// made in Go: only segments strictly older than the checkpoint REDO are removed.
func TestPruneWALOnPVC_DeletesOnlyOlderSegmentsWhenCaughtUp(t *testing.T) {
	r := &execRouter{
		controlData: controlDataREDO5,
		replayLSN:   map[string]string{"pg-2": "0/6000000"}, // past 0/5000028 — caught up
		segments: []string{
			walPath("000000010000000000000003"),
			walPath("000000010000000000000004"),
			walPath("000000010000000000000005"), // == REDO, keep
			walPath("000000010000000000000006"), // > REDO, keep
		},
	}
	defer k8s.SetExecHookForTest(r.hook())()

	if err := testPruner(false).pruneWALOnPVC(context.Background(), "pg-prune", "ns", []string{"pg-2"}, true); err != nil {
		t.Fatalf("prune should succeed when the replica is caught up: %v", err)
	}
	if !r.rmCalled {
		t.Fatal("expected rm to be issued for the older segments")
	}
	got := strings.Join(r.rmArgs, " ")
	for _, keep := range []string{"000000010000000000000005", "000000010000000000000006"} {
		if strings.Contains(got, keep) {
			t.Fatalf("deleted a segment >= the checkpoint REDO (%s) — data loss risk; rm args: %v", keep, r.rmArgs)
		}
	}
	for _, del := range []string{"000000010000000000000003", "000000010000000000000004"} {
		if !strings.Contains(got, del) {
			t.Fatalf("did not delete pre-checkpoint segment %s; rm args: %v", del, r.rmArgs)
		}
	}
}

// TestPruneWALOnPVC_NoReplicasAbortsWithoutForce: with nothing to verify against, the
// prune refuses unless --force.
func TestPruneWALOnPVC_NoReplicasAbortsWithoutForce(t *testing.T) {
	r := &execRouter{controlData: controlDataREDO5}
	defer k8s.SetExecHookForTest(r.hook())()

	err := testPruner(false).pruneWALOnPVC(context.Background(), "pg-prune", "ns", nil, true)
	if err == nil || !strings.Contains(err.Error(), "no ready replicas") {
		t.Fatalf("prune must refuse with no replica to verify against (without --force), got: %v", err)
	}
	if r.rmCalled {
		t.Fatal("prune deleted WAL with no safety evidence and no --force")
	}
}

// TestPruneWALOnPVC_ForceOverridesLaggingReplica: --force proceeds past a lagging replica.
func TestPruneWALOnPVC_ForceOverridesLaggingReplica(t *testing.T) {
	r := &execRouter{
		controlData: controlDataREDO5,
		replayLSN:   map[string]string{"pg-2": "0/4000000"}, // behind, but --force
		segments:    []string{walPath("000000010000000000000003")},
	}
	defer k8s.SetExecHookForTest(r.hook())()

	if err := testPruner(true).pruneWALOnPVC(context.Background(), "pg-prune", "ns", []string{"pg-2"}, true); err != nil {
		t.Fatalf("--force should override a lagging replica: %v", err)
	}
	if !r.rmCalled {
		t.Fatal("--force should have proceeded to prune")
	}
}

// TestPruneWALOnPVC_NonPrimaryReliefSkipsReplicaGate is the P3.4 relief path: a trapped
// non-primary authority has no downstream streamer, so verifyReplicas=false must skip the
// replica-caughtup gate entirely (no replay query) and still prune only pre-REDO WAL —
// with NO ready replica present and NO --force (which the primary path would refuse).
func TestPruneWALOnPVC_NonPrimaryReliefSkipsReplicaGate(t *testing.T) {
	r := &execRouter{
		controlData: controlDataREDO5,
		segments: []string{
			walPath("000000010000000000000003"),
			walPath("000000010000000000000004"),
			walPath("000000010000000000000005"), // == REDO, keep
		},
	}
	defer k8s.SetExecHookForTest(r.hook())()

	// force=false, no replicas — the primary path would ABORT here; the relief path proceeds.
	if err := testPruner(false).pruneWALOnPVC(context.Background(), "pg-prune", "ns", nil, false); err != nil {
		t.Fatalf("non-primary authority relief should proceed without replicas/force: %v", err)
	}
	if !r.rmCalled {
		t.Fatal("relief should have pruned the pre-REDO segments")
	}
	if strings.Contains(strings.Join(r.rmArgs, " "), "000000010000000000000005") {
		t.Fatalf("relief deleted the REDO segment — data loss risk; rm args: %v", r.rmArgs)
	}
}

// TestAssertReliefEligible covers the P3.4 authority gate for a NON-primary target: the
// proven authority (or authoritative classification) is relievable; a divergence needs
// --force (operator-adjudicated survivor); a plain replica is refused (re-clone instead).
func TestAssertReliefEligible(t *testing.T) {
	tr := func(dc model.DataComparison) *model.TriageResult { return &model.TriageResult{DataComparison: dc} }
	a := func(class model.Classification) *model.InstanceAssessment {
		return &model.InstanceAssessment{Pod: "pg-2", Classification: class}
	}

	// Proven authority (MostAdvanced) → allowed.
	if err := testPruner(false).assertReliefEligible("pg-2", a(""), tr(model.DataComparison{MostAdvanced: "pg-2", Authority: model.AuthorityLeaderNotPrimary})); err != nil {
		t.Fatalf("proven authority must be relievable, got: %v", err)
	}
	// Authoritative classification → allowed even if MostAdvanced is unset.
	if err := testPruner(false).assertReliefEligible("pg-2", a(model.ClassAuthoritative), tr(model.DataComparison{})); err != nil {
		t.Fatalf("authoritative classification must be relievable, got: %v", err)
	}
	// Diverged + no force → refused (must adjudicate first).
	if err := testPruner(false).assertReliefEligible("pg-2", a(model.ClassUnknown), tr(model.DataComparison{Authority: model.AuthorityDiverged})); err == nil || !strings.Contains(err.Error(), "DIVERGED") {
		t.Fatalf("diverged relief must require --force, got: %v", err)
	}
	// Diverged + force → allowed (operator-designated survivor).
	if err := testPruner(true).assertReliefEligible("pg-2", a(model.ClassUnknown), tr(model.DataComparison{Authority: model.AuthorityDiverged})); err != nil {
		t.Fatalf("diverged relief with --force must proceed, got: %v", err)
	}
	// Plain non-authority replica → refused (re-clone, don't prune).
	if err := testPruner(true).assertReliefEligible("pg-2", a(model.ClassDisposable), tr(model.DataComparison{MostAdvanced: "pg-1"})); err == nil || !strings.Contains(err.Error(), "re-cloned") {
		t.Fatalf("a disposable non-authority replica must be refused (re-clone, not prune), got: %v", err)
	}
}

func TestParseLSN(t *testing.T) {
	a, err := parseLSN("0/5000028")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseLSN("A/36000028")
	if err != nil {
		t.Fatal(err)
	}
	if !(a < b) {
		t.Fatalf("expected 0/5000028 < A/36000028, got %d vs %d", a, b)
	}
	if _, err := parseLSN("not-an-lsn"); err == nil {
		t.Fatal("expected malformed LSN to error")
	}
}
