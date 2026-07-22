package triage

import "testing"

// sp builds a switch point from a PostgreSQL LSN string, e.g. sp(8, "0/3E8").
func sp(parentTL int64, lsn string) switchPoint {
	return switchPoint{ParentTimeline: parentTL, SwitchLSN: parseLSNValue(lsn)}
}

func lsn(s string) int64 { return parseLSNValue(s) }

// read builds a Read instance.
func read(pod string, primary bool, tl int64, ckpt string, sw ...switchPoint) authorityInput {
	return authorityInput{
		Pod: pod, IsPrimary: primary, ReadState: ReadStateRead,
		Timeline: tl, CheckpointLSN: lsn(ckpt), Switches: sw,
	}
}

func TestParseTimelineHistory(t *testing.T) {
	raw := "1\t0/3000000\tno recovery target\n" +
		"2\t0/5000000\tafter failover\n" +
		"\n" + // blank
		"garbage line without lsn\n"
	got := parseTimelineHistory(raw)
	if len(got) != 2 {
		t.Fatalf("want 2 switch points, got %d: %+v", len(got), got)
	}
	if got[0] != sp(1, "0/3000000") || got[1] != sp(2, "0/5000000") {
		t.Fatalf("parsed switch points wrong: %+v", got)
	}
}

func TestAuthority_HealthySameTimeline_PrimaryLeads(t *testing.T) {
	// Both on timeline 2, identical history; primary is furthest ahead.
	insts := []authorityInput{
		read("pg-1", true, 2, "0/9000", sp(1, "0/5000")),
		read("pg-2", false, 2, "0/8000", sp(1, "0/5000")),
		read("pg-3", false, 2, "0/8000", sp(1, "0/5000")),
	}
	d := determineAuthority(insts)
	if !d.Determinable || d.Diverged {
		t.Fatalf("want determinable non-diverged, got %+v", d)
	}
	if d.Leader != "pg-1" {
		t.Fatalf("want leader pg-1, got %q", d.Leader)
	}
}

func TestAuthority_ReplicaBehindLowerTimeline_PureAncestor(t *testing.T) {
	// A post-failover cluster: primary on TL2 (forked from TL1 at 0/5000), a lagging
	// replica still on TL1 whose checkpoint is BEFORE the fork — a pure ancestor.
	insts := []authorityInput{
		read("pg-1", true, 2, "0/9000", sp(1, "0/5000")),
		read("pg-2", false, 1, "0/4000"), // TL1, no history, checkpoint < fork
	}
	d := determineAuthority(insts)
	if !d.Determinable || d.Diverged || d.Leader != "pg-1" {
		t.Fatalf("want primary pg-1 to lead decisively, got %+v", d)
	}
}

func TestAuthority_StaleRestore_LowerTimelineWins(t *testing.T) {
	// THE case that motivated this: the primary is a fresh restore bumped to TL9,
	// forked from TL8 at 0/1000 with NO writes since (checkpoint sits at the fork).
	// The golden replica on TL8 holds committed WAL well past the fork. The higher
	// timeline NUMBER must NOT win — the replica is the true authority.
	shared := []switchPoint{sp(1, "0/100"), sp(2, "0/200"), sp(3, "0/300"),
		sp(4, "0/400"), sp(5, "0/500"), sp(6, "0/600"), sp(7, "0/700")}
	primary := read("pg-1", true, 9, "0/1000", append(append([]switchPoint{}, shared...), sp(8, "0/1000"))...)
	replica := read("pg-2", false, 8, "0/9000", shared...)
	d := determineAuthority([]authorityInput{primary, replica})
	if !d.Determinable {
		t.Fatalf("want determinable, got %+v", d)
	}
	if d.Diverged {
		t.Fatalf("must NOT be divergence — the restore is dataless past the fork: %+v", d)
	}
	if d.Leader != "pg-2" {
		t.Fatalf("want the lower-timeline golden replica pg-2 to win, got %q", d.Leader)
	}
}

func TestAuthority_TrueDivergence_BothWrotePastFork(t *testing.T) {
	// The boundary-postgres shape: the restored primary on TL9 ALSO accepted writes
	// past the fork, and the golden replica on TL8 has its own writes past the fork.
	// Two lineages both carry committed data past the shared fork → no safe winner.
	shared := []switchPoint{sp(1, "0/100"), sp(2, "0/200"), sp(3, "0/300"),
		sp(4, "0/400"), sp(5, "0/500"), sp(6, "0/600"), sp(7, "0/700")}
	primary := read("pg-1", true, 9, "0/5000", append(append([]switchPoint{}, shared...), sp(8, "0/1000"))...)
	replica := read("pg-2", false, 8, "0/9000", shared...)
	d := determineAuthority([]authorityInput{primary, replica})
	if !d.Determinable || !d.Diverged {
		t.Fatalf("want determinable divergence, got %+v", d)
	}
	if d.Leader != "" {
		t.Fatalf("divergence must name no leader, got %q", d.Leader)
	}
	if len(d.Divergences) == 0 {
		t.Fatalf("want a divergence explanation")
	}
}

func TestAuthority_UnreadInstanceBlocks(t *testing.T) {
	// A readable primary and replica would give a clean answer, but a third instance
	// with data-that-could-not-be-read makes the whole decision undeterminable.
	insts := []authorityInput{
		read("pg-1", true, 2, "0/9000", sp(1, "0/5000")),
		read("pg-2", false, 2, "0/8000", sp(1, "0/5000")),
		{Pod: "pg-3", ReadState: ReadStateUnread, UnreadReason: "Bound PVC, probe unschedulable (node full)"},
	}
	d := determineAuthority(insts)
	if d.Determinable {
		t.Fatalf("an unread instance must block the decision, got %+v", d)
	}
	if len(d.Blockers) != 1 {
		t.Fatalf("want 1 blocker, got %v", d.Blockers)
	}
}

func TestAuthority_AbsentNoDataDoesNotBlock(t *testing.T) {
	// A provably-empty instance (no PVC / empty volume) has nothing to lose and must
	// not block; the decision proceeds over the instances that DO have data.
	insts := []authorityInput{
		read("pg-1", true, 2, "0/9000", sp(1, "0/5000")),
		{Pod: "pg-2", ReadState: ReadStateAbsentNoData},
	}
	d := determineAuthority(insts)
	if !d.Determinable || d.Leader != "pg-1" {
		t.Fatalf("want determinable leader pg-1 despite an absent peer, got %+v", d)
	}
}

func TestAuthority_AllAbsent_NoAuthorityButDeterminable(t *testing.T) {
	insts := []authorityInput{
		{Pod: "pg-1", ReadState: ReadStateAbsentNoData},
		{Pod: "pg-2", ReadState: ReadStateAbsentNoData},
	}
	d := determineAuthority(insts)
	if !d.Determinable || d.Leader != "" || d.Diverged {
		t.Fatalf("all-empty cluster should be determinable with no authority, got %+v", d)
	}
}

func TestAuthority_Tie_PrefersPrimary(t *testing.T) {
	// Identical position on two instances → either is safe; we name the primary.
	insts := []authorityInput{
		read("pg-2", false, 2, "0/9000", sp(1, "0/5000")),
		read("pg-1", true, 2, "0/9000", sp(1, "0/5000")),
	}
	d := determineAuthority(insts)
	if !d.Determinable || d.Leader != "pg-1" {
		t.Fatalf("want the primary named on a tie, got %+v", d)
	}
}

func TestAuthority_TimelineOne_NoHistory(t *testing.T) {
	// The everyday healthy cluster never left timeline 1: no history files, rank by LSN.
	insts := []authorityInput{
		read("pg-1", true, 1, "0/9000"),
		read("pg-2", false, 1, "0/8000"),
	}
	d := determineAuthority(insts)
	if !d.Determinable || d.Diverged || d.Leader != "pg-1" {
		t.Fatalf("want primary pg-1 to lead, got %+v", d)
	}
}

func TestAuthority_EarlierFork_BothPast_Diverges(t *testing.T) {
	// Two instances that forked at an EARLIER shared ancestor (different switch at the
	// same depth), both with data past it → divergence.
	a := read("pg-1", true, 3, "0/9000", sp(1, "0/100"), sp(2, "0/500"))
	b := read("pg-2", false, 3, "0/9000", sp(1, "0/100"), sp(2, "0/800"))
	d := determineAuthority([]authorityInput{a, b})
	if !d.Determinable || !d.Diverged {
		t.Fatalf("want divergence on an earlier fork, got %+v", d)
	}
}
