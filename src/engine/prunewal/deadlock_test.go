package prunewal

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/k8s"
)

// dlRouter routes the deadlock-recover Go-driven exec calls and records what was issued.
type dlRouter struct {
	controldata   string
	replayOut     string
	relocated     bool
	replayed      bool
	archiveCalled bool
	archiveCutoff string
	movedback     bool
}

func (r *dlRouter) hook() func(ctx context.Context, pod, ns, c string, cmd []string) (*k8s.ExecResult, error) {
	return func(ctx context.Context, pod, ns, c string, cmd []string) (*k8s.ExecResult, error) {
		joined := strings.Join(cmd, " ")
		switch {
		case cmd[0] == "pg_controldata":
			return &k8s.ExecResult{Stdout: r.controldata}, nil
		case cmd[0] == "pg_archivecleanup":
			r.archiveCalled = true
			if len(cmd) >= 3 {
				r.archiveCutoff = cmd[2]
			}
			return &k8s.ExecResult{}, nil
		case strings.Contains(joined, "postgres --single"):
			r.replayed = true
			return &k8s.ExecResult{Stdout: r.replayOut}, nil
		case strings.Contains(joined, "ln -s"): // the relocate script
			r.relocated = true
			return &k8s.ExecResult{Stdout: "/dev/rbd0 974 65 893 7% /x"}, nil
		case strings.Contains(joined, "mv") && strings.Contains(joined, "pg_wal"): // moveback
			r.movedback = true
			return &k8s.ExecResult{Stdout: "/dev/rbd0 974 81 877 9% /x"}, nil
		default:
			return &k8s.ExecResult{}, nil
		}
	}
}

const dlShutDown = `pg_control version number:            1300
Database cluster state:               shut down
Latest checkpoint location:           8F/27000028
Latest checkpoint's REDO location:    8F/27000028
Latest checkpoint's REDO WAL file:    000000080000008F00000027
Latest checkpoint's TimeLineID:       8`

// Happy path: relocate → replay → verify shut down → archivecleanup at the POST-replay
// REDO WAL file → move back. The cutoff must come from the control data read AFTER replay.
func TestDeadlockRecoverOnPVC_HappyPath(t *testing.T) {
	r := &dlRouter{controldata: dlShutDown, replayOut: "redo done at 8F/27; checkpoint complete"}
	defer k8s.SetExecHookForTest(r.hook())()

	if _, _, err := testPruner(false).deadlockRecoverOnPVC(context.Background(), "pod", "ns"); err != nil {
		t.Fatalf("happy path must succeed: %v", err)
	}
	if !r.relocated || !r.replayed || !r.archiveCalled || !r.movedback {
		t.Fatalf("sequence incomplete: relocated=%v replayed=%v archive=%v moveback=%v",
			r.relocated, r.replayed, r.archiveCalled, r.movedback)
	}
	if r.archiveCutoff != "000000080000008F00000027" {
		t.Fatalf("archivecleanup cutoff must be the post-replay REDO WAL file, got %q", r.archiveCutoff)
	}
}

// If the instance is NOT cleanly shut down after replay, it must refuse to trim WAL
// (no archivecleanup, no move-back) and leave the datadir fenced for inspection.
func TestDeadlockRecoverOnPVC_RefusesTrimIfNotShutDown(t *testing.T) {
	notClean := strings.Replace(dlShutDown, "shut down", "in crash recovery", 1)
	r := &dlRouter{controldata: notClean, replayOut: "redo in progress"}
	defer k8s.SetExecHookForTest(r.hook())()

	_, _, err := testPruner(false).deadlockRecoverOnPVC(context.Background(), "pod", "ns")
	if err == nil || !strings.Contains(err.Error(), "shut down") {
		t.Fatalf("must refuse when not cleanly shut down, got: %v", err)
	}
	if r.archiveCalled || r.movedback {
		t.Fatal("must NOT trim WAL or move back when the replay did not reach a clean shutdown")
	}
}

// A PANIC/FATAL in the replay output must abort before any WAL trim.
func TestDeadlockRecoverOnPVC_AbortsOnReplayPanic(t *testing.T) {
	r := &dlRouter{controldata: dlShutDown, replayOut: "PANIC: could not write to file: No space left on device"}
	defer k8s.SetExecHookForTest(r.hook())()

	_, _, err := testPruner(false).deadlockRecoverOnPVC(context.Background(), "pod", "ns")
	if err == nil || !strings.Contains(err.Error(), "PANIC") {
		t.Fatalf("must abort on replay PANIC, got: %v", err)
	}
	if r.archiveCalled {
		t.Fatal("must NOT run archivecleanup after a replay PANIC")
	}
}

func TestParseControlState(t *testing.T) {
	state, redo, _, _ := parseControlState(dlShutDown)
	if state != "shut down" {
		t.Fatalf("state=%q, want \"shut down\"", state)
	}
	if redo != "000000080000008F00000027" {
		t.Fatalf("redoWAL=%q, want 000000080000008F00000027", redo)
	}
}
