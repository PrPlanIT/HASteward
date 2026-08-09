package triage

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// fakeInstance models one postgres instance as table → the multiset of its rows'
// content hashes. fakeExec answers discoverTables / sigOf / hashesOf from it exactly
// as real psql would, so the engine is exercised end-to-end without a database.
type fakeInstance map[string][]string

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func tableFromQuery(q string) string {
	// The sig query nests "FROM (SELECT ... FROM <tbl> t) s" — the real table follows
	// the LAST FROM; hashesOf has a single FROM. LastIndex handles both.
	idx := strings.LastIndex(q, "FROM ")
	if idx < 0 {
		return ""
	}
	return strings.SplitN(q[idx+len("FROM "):], " t", 2)[0]
}

func fakeExec(inst fakeInstance) instanceExec {
	return func(q string) (string, error) {
		switch {
		case strings.Contains(q, "pg_class"): // discoverTables
			var ts []string
			for t := range inst {
				ts = append(ts, t)
			}
			sort.Strings(ts)
			return strings.Join(ts, "\n"), nil
		case strings.Contains(q, "string_agg(rh"): // sigOf
			rows := inst[tableFromQuery(q)]
			sorted := append([]string{}, rows...)
			sort.Strings(sorted)
			return fmt.Sprintf("%d|%s", len(rows), md5hex(strings.Join(sorted, ","))), nil
		case strings.Contains(q, "md5(t.*::text) FROM"): // hashesOf
			return strings.Join(inst[tableFromQuery(q)], "\n"), nil
		}
		return "", fmt.Errorf("unexpected query: %s", q)
	}
}

func mkGeneric(t *testing.T, insts map[string]fakeInstance) (contentComparator, func(a, b string) []tableDivergence) {
	t.Helper()
	execs := map[string]instanceExec{}
	for pod, inst := range insts {
		execs[pod] = fakeExec(inst)
	}
	return genericContentComparator(execs)
}

func TestDiffTableMath(t *testing.T) {
	ah := map[string]int64{"r1": 1, "r2": 1, "x": 1}
	bh := map[string]int64{"r1": 1, "r2": 1, "y": 2}
	d := diffTable("t", ah, bh)
	if d.aMissing != 2 || d.bMissing != 1 {
		t.Fatalf("want aMissing=2 bMissing=1, got %+v", d)
	}
}

// One instance holds every row the other does, plus more, across every table → superset.
func TestGeneric_Superset(t *testing.T) {
	cmp, _ := mkGeneric(t, map[string]fakeInstance{
		"pg-1": {"public.a": {"r1", "r2", "r3"}, "public.b": {"q1", "q2"}},
		"pg-2": {"public.a": {"r1", "r2"}, "public.b": {"q1"}},
	})
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-2"}); got != contentAContainsB {
		t.Fatalf("want contentAContainsB, got %v", got)
	}
	if got := cmp(authorityInput{Pod: "pg-2"}, authorityInput{Pod: "pg-1"}); got != contentBContainsA {
		t.Fatalf("reverse: want contentBContainsA, got %v", got)
	}
}

func TestGeneric_Equal(t *testing.T) {
	cmp, _ := mkGeneric(t, map[string]fakeInstance{
		"pg-1": {"public.a": {"r1", "r2"}},
		"pg-2": {"public.a": {"r2", "r1"}}, // same multiset, different order
	})
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-2"}); got != contentEqual {
		t.Fatalf("want contentEqual, got %v", got)
	}
}

// The zitadel shape: a shared business prefix, but each instance carries its OWN
// restart churn the other lacks. The engine must call this CROSSED (no safe pick) and
// localize the divergence to the event table — never guess a winner without a witness.
func TestGeneric_ZitadelShape_Crossed(t *testing.T) {
	business := []string{"e1", "e2", "e3", "e4", "e5"}
	cmp, summarize := mkGeneric(t, map[string]fakeInstance{
		"pg-1": {"eventstore.events2": append(append([]string{}, business...), "churn1a", "churn1b"),
			"projections.users": {"u1", "u2"}},
		"pg-2": {"eventstore.events2": append(append([]string{}, business...), "churn2a"),
			"projections.users": {"u1", "u2"}},
	})
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-2"}); got != contentCrossed {
		t.Fatalf("want contentCrossed for the churn shape, got %v", got)
	}
	divs := summarize("pg-1", "pg-2")
	if len(divs) != 1 || divs[0].table != "eventstore.events2" {
		t.Fatalf("divergence must localize to the event table, got %+v", divs)
	}
	// pg-1 has 2 churn rows pg-2 lacks; pg-2 has 1 churn row pg-1 lacks.
	if divs[0].bMissing != 2 || divs[0].aMissing != 1 {
		t.Fatalf("want aMissing=1 bMissing=2, got %+v", divs[0])
	}
}

// A pod with no exec (unreadable/down and not opened) → contentUnknown (fail closed).
func TestGeneric_UnknownPeer(t *testing.T) {
	cmp, _ := mkGeneric(t, map[string]fakeInstance{
		"pg-1": {"public.a": {"r1"}},
	})
	if got := cmp(authorityInput{Pod: "pg-1"}, authorityInput{Pod: "pg-missing"}); got != contentUnknown {
		t.Fatalf("want contentUnknown for a peer with no exec, got %v", got)
	}
}
