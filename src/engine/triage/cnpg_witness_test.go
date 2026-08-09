package triage

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/k8s"
)

// witnessHook builds a fake psql exec hook. base maps pod → "count|maxPos|hash"
// (the full-fingerprint reply); upto maps pod → (peerMaxLiteral → "count|hash") for
// the containment probe. An empty/absent reply models a down instance (base fails).
func witnessHook(base map[string]string, upto map[string]map[string]string) func(context.Context, string, string, string, []string) (*k8s.ExecResult, error) {
	return func(_ context.Context, pod, _, _ string, cmd []string) (*k8s.ExecResult, error) {
		q := cmd[len(cmd)-1]
		if strings.Contains(q, "max(") { // full-fingerprint query
			return &k8s.ExecResult{Stdout: base[pod]}, nil
		}
		for lit, out := range upto[pod] { // containment probe: match "<= <lit>"
			if strings.Contains(q, "<= "+lit) {
				return &k8s.ExecResult{Stdout: out}, nil
			}
		}
		return &k8s.ExecResult{Stdout: ""}, nil
	}
}

func mkComparator(t *testing.T, base map[string]string, upto map[string]map[string]string, pods ...string) contentComparator {
	t.Helper()
	restore := k8s.SetExecHookForTest(witnessHook(base, upto))
	t.Cleanup(restore)
	spec := witnessSpec{DB: "app", Table: "eventstore.events2", Position: "position", AppendOnly: true}
	return lazyContentComparator(spec, func() map[string]witnessBase {
		return collectLiveBases(context.Background(), "ns", spec, pods)
	})
}

// pg-1 holds every row pg-2 does plus more: pg-1's prefix up to pg-2's max (90)
// hash-matches pg-2's full set. Containment proven.
func TestContent_AContainsB(t *testing.T) {
	cmp := mkComparator(t,
		map[string]string{"pg-1": "10|100|H1full", "pg-2": "8|90|H2full"},
		map[string]map[string]string{
			"pg-1": {"90": "8|H2full"},  // pg-1 rows ≤ 90 == pg-2's full
			"pg-2": {"100": "8|H2full"}, // pg-2 rows ≤ 100 == its own 8 (≠ pg-1's 10) → not contained
		}, "pg-1", "pg-2")
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-2"}); got != contentAContainsB {
		t.Fatalf("want contentAContainsB, got %v", got)
	}
}

// Same count and max position but different content hash over the shared range →
// each holds rows the other lacks → true crossing, no winner.
func TestContent_Crossed(t *testing.T) {
	cmp := mkComparator(t,
		map[string]string{"pg-1": "10|100|H1", "pg-2": "10|100|H2"},
		map[string]map[string]string{
			"pg-1": {"100": "10|H1"}, // pg-1 ≤100 = its own (10,H1) ≠ pg-2 (10,H2)
			"pg-2": {"100": "10|H2"}, // symmetric
		}, "pg-1", "pg-2")
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-2"}); got != contentCrossed {
		t.Fatalf("want contentCrossed, got %v", got)
	}
}

// Identical fingerprints → equal; either is a safe authority.
func TestContent_Equal(t *testing.T) {
	cmp := mkComparator(t,
		map[string]string{"pg-1": "10|100|HX", "pg-2": "10|100|HX"},
		map[string]map[string]string{
			"pg-1": {"100": "10|HX"},
			"pg-2": {"100": "10|HX"},
		}, "pg-1", "pg-2")
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-2"}); got != contentEqual {
		t.Fatalf("want contentEqual, got %v", got)
	}
}

// A down instance (no fingerprint reply) is not content-readable → the pair stays
// contentUnknown, so the WAL divergence is never cleared past a node we couldn't read.
func TestContent_DownInstanceUnknown(t *testing.T) {
	cmp := mkComparator(t,
		map[string]string{"pg-1": "10|100|H1"}, // pg-3 absent → base fails
		map[string]map[string]string{},
		"pg-1", "pg-3")
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-3"}); got != contentUnknown {
		t.Fatalf("want contentUnknown for an unreadable peer, got %v", got)
	}
}

// deepContentBase parses a down instance's fingerprint from the standalone-copy reads
// and precomputes cuts for the given peer maxes. The sh -c prep/cleanup calls return
// empty (success); psql base/cut calls are served by the same witnessHook.
func TestDeepContentBase(t *testing.T) {
	restore := k8s.SetExecHookForTest(witnessHook(
		map[string]string{"pg-3": "37814|1786086299|H3"},
		map[string]map[string]string{"pg-3": {"1786112415": "37814|H3"}},
	))
	t.Cleanup(restore)
	spec := witnessSpec{DB: "zitadel", Table: "eventstore.events2", Position: "position", AppendOnly: true}
	b := deepContentBase(context.Background(), "ns", "pg-3", spec, []string{"1786112415"})
	if !b.ok || b.count != 37814 || b.maxPos != "1786086299" || b.hash != "H3" {
		t.Fatalf("bad down base: %+v", b)
	}
	if c, h, ok := b.cut("1786112415"); !ok || c != 37814 || h != "H3" {
		t.Fatalf("precomputed cut wrong: c=%d h=%s ok=%v", c, h, ok)
	}
	if _, _, ok := b.cut("999999"); ok {
		t.Fatalf("a non-precomputed peer max must be unknown (fail closed)")
	}
}
