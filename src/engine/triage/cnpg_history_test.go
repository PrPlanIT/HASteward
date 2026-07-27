package triage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// histFiles builds the raw blob the probe/exec now emits: every pg_wal/*.history
// file for a node on timeline len(forks)+1, each under a "###<filename>" marker. File
// 0000000n.history repeats the first n-1 switch lines — exactly the concatenation
// that corrupted the lineage before the fix. forks[k] is the switch INTO timeline k+2.
func histFiles(forks ...string) string {
	tl := len(forks) + 1
	var b strings.Builder
	for n := 2; n <= tl; n++ {
		fmt.Fprintf(&b, "###%08X.history\n", n)
		for k := 1; k < n; k++ {
			fmt.Fprintf(&b, "%d\t%s\tno recovery target specified\n", k, forks[k-1])
		}
	}
	return b.String()
}

func TestHistoryForTimeline(t *testing.T) {
	// A TL9 node's blob (files 2..9). Selecting 9 must yield the 8-line clean lineage
	// ending at the TL8→TL9 switch; selecting 8 yields the 7-line TL8 lineage.
	blob := histFiles("11/840000A0", "2C/960013E8", "72/D8007388", "78/59032EF0",
		"8E/9E0000A0", "8E/E40030C0", "8E/EE029E80", "2C/99000000")

	tl9 := parseTimelineHistory(historyForTimeline(blob, 9))
	if len(tl9) != 8 || tl9[7] != sp(8, "2C/99000000") {
		t.Fatalf("TL9 lineage wrong: %+v", tl9)
	}
	tl8 := parseTimelineHistory(historyForTimeline(blob, 8))
	if len(tl8) != 7 || tl8[6] != sp(7, "8E/EE029E80") {
		t.Fatalf("TL8 lineage wrong: %+v", tl8)
	}
	// No-marker blob is assumed already-clean and returned as-is (hand-built history).
	if got := historyForTimeline("1\t0/500\tx", 2); got != "1\t0/500\tx" {
		t.Fatalf("unmarked passthrough failed: %q", got)
	}
	// A timeline with no matching file → empty (→ Unread upstream, fail closed).
	if got := historyForTimeline(blob, 12); got != "" {
		t.Fatalf("missing-file timeline should be empty, got %q", got)
	}
}

// TestCnpgComparison_MultiRestoreForkLSN is the boundary-postgres regression: a real
// multi-restore cluster where each node carries many .history files. Before the fix,
// blindly concatenating them made the reported fork the ARTIFACT 11/840000A0 (the
// first line of the next file) and reached "diverged" only by a fail-closed structure
// mismatch. After the fix the lineage is read from the current timeline's file, so the
// fork is the REAL TL8→TL9 switch 2C/99000000 — and it's still (correctly) divergent
// because both checkpoints sit past it.
func TestCnpgComparison_MultiRestoreForkLSN(t *testing.T) {
	forks9 := []string{"11/840000A0", "2C/960013E8", "72/D8007388", "78/59032EF0",
		"8E/9E0000A0", "8E/E40030C0", "8E/EE029E80", "2C/99000000"}
	forks8 := forks9[:7] // TL8 shares the lineage up to the TL7→TL8 switch

	cd := func(pod, tl, lsn string, forks []string) controlData {
		return controlData{Pod: pod, Timeline: tl, CheckpointLocation: lsn,
			HistoryRaw: histFiles(forks...), Source: "exec", Reachable: true, PGDataPresent: "yes"}
	}
	data := &cnpgTriageData{
		controlData: []controlData{
			cd("boundary-postgres-3", "9", "2E/BE000070", forks9), // TL9 restore branch
			cd("boundary-postgres-2", "8", "8F/25000028", forks8), // TL8 golden branch
		},
		pvcStates: map[string]string{"boundary-postgres-3": "Bound", "boundary-postgres-2": "Bound"},
	}
	data.primaryControlData = &data.controlData[0]
	data.primaryTimeline = "9"

	cmp := cnpgCrossInstanceComparison(data, "boundary-postgres-3")

	if cmp.SafeToHeal || cmp.Authority != model.AuthorityDiverged {
		t.Fatalf("want diverged+unsafe, got authority=%q safe=%v", cmp.Authority, cmp.SafeToHeal)
	}
	if !containsAny(cmp.SplitBrainDetails, "2C/99000000") {
		t.Fatalf("fork LSN must be the REAL TL8→TL9 switch 2C/99000000, got %v", cmp.SplitBrainDetails)
	}
	if containsAny(cmp.SplitBrainDetails, "11/840000A0") {
		t.Fatalf("11/840000A0 is the concatenation artifact and must NOT be the reported fork: %v", cmp.SplitBrainDetails)
	}
	// #1: the per-branch WAL volume past the fork must be surfaced to inform the
	// manual lineage choice — TL8 (golden, ~394 GiB past fork) vs TL9 (~8.6 GiB).
	if !containsAny(cmp.SplitBrainDetails, "WAL past fork") {
		t.Fatalf("divergence must surface per-branch WAL-past-fork volume, got %v", cmp.SplitBrainDetails)
	}
	if !containsAny(cmp.SplitBrainDetails, "394.2 GB") || !containsAny(cmp.SplitBrainDetails, "8.6 GB") {
		t.Fatalf("expected the TL8 (394.2 GB) and TL9 (8.6 GB) WAL-past-fork volumes, got %v", cmp.SplitBrainDetails)
	}
	// Part B: when a write ledger was sampled from the running primary, it rides along
	// on the divergence so the churn-vs-real evidence is presented, not hand-gathered.
	data.writeActivityDB = "boundary"
	data.writeActivity = []string{
		"job_run|900000|12|900000|3",
		"server_controller|4500|4500|0|150",
		"session|2|1|0|2",
	}
	cmp2 := cnpgCrossInstanceComparison(data, "boundary-postgres-3")
	if !containsAny(cmp2.SplitBrainDetails, "db=boundary") ||
		!containsAny(cmp2.SplitBrainDetails, "job_run") ||
		!containsAny(cmp2.SplitBrainDetails, "session") {
		t.Fatalf("divergence must carry the primary write ledger, got %v", cmp2.SplitBrainDetails)
	}
}

func TestFormatWriteLedger(t *testing.T) {
	// Nothing sampled → no section (caller omits it cleanly).
	if got := formatWriteLedger(&cnpgTriageData{}); got != nil {
		t.Fatalf("empty ledger must be nil, got %v", got)
	}
	d := &cnpgTriageData{writeActivityDB: "app", writeActivity: []string{
		"job_run|900000|12|900000|3",
		"malformed_row_no_pipes",
	}}
	got := formatWriteLedger(d)
	if len(got) != 3 { // header + 2 rows
		t.Fatalf("want header+2 rows, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "db=app") {
		t.Fatalf("header must name the db, got %q", got[0])
	}
	if !strings.Contains(got[1], "job_run: ins=900000 upd=12 del=900000, live=3") {
		t.Fatalf("row formatting wrong: %q", got[1])
	}
	if !strings.Contains(got[2], "malformed_row_no_pipes") {
		t.Fatalf("malformed rows must pass through verbatim, got %q", got[2])
	}
}

func TestIsSafeDBIdent(t *testing.T) {
	for _, ok := range []string{"boundary", "app_1", "my-db", "A0"} {
		if !isSafeDBIdent(ok) {
			t.Errorf("%q should be safe", ok)
		}
	}
	for _, bad := range []string{"", "db; DROP", "a b", "quote'd", strings.Repeat("x", 64)} {
		if isSafeDBIdent(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestFormatWALPastFork(t *testing.T) {
	cases := []struct {
		ckpt, fork int64
		want       string
	}{
		{2 << 30, 0, "2.0 GB"},
		{5 << 20, 0, "5.0 MB"},
		{3 << 10, 0, "3.0 KB"},
		{512, 0, "512 B"},
		{100, 100, "0 B"},  // at the fork — dataless branch
		{50, 100, "0 B"},   // behind the fork — clamp to 0
		{lsn("8F/25000028"), lsn("2C/99000000"), "394.2 GB"},
		{lsn("2E/BE000070"), lsn("2C/99000000"), "8.6 GB"},
	}
	for _, c := range cases {
		if got := formatWALPastFork(c.ckpt, c.fork); got != c.want {
			t.Errorf("formatWALPastFork(%d,%d) = %q, want %q", c.ckpt, c.fork, got, c.want)
		}
	}
}
