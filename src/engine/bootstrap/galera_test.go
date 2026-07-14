package bootstrap

import "testing"

// A single-lineage belly-up cluster: one group, best node = highest recovered
// seqno (ties broken by lastCommitted). This is the clean case the belly-up path
// bootstraps automatically.
func TestBuildLineageGroups_SingleLineage(t *testing.T) {
	rec := map[string]wsrepRecoverResult{
		"osticket-mariadb-0": {UUID: "aaa", Seqno: 552461, LastCommitted: 552461, Valid: true},
		"osticket-mariadb-1": {UUID: "aaa", Seqno: 552481, LastCommitted: 552481, Valid: true},
		"osticket-mariadb-2": {UUID: "aaa", Seqno: 552481, LastCommitted: 552480, Valid: true},
	}
	groups := buildLineageGroups(rec)
	if len(groups) != 1 {
		t.Fatalf("want 1 lineage group, got %d", len(groups))
	}
	if groups[0].MaxSeqno != 552481 {
		t.Errorf("MaxSeqno: want 552481, got %d", groups[0].MaxSeqno)
	}
	if groups[0].BestNode != "osticket-mariadb-1" { // higher lastCommitted at the tied top seqno
		t.Errorf("BestNode: want osticket-mariadb-1, got %s", groups[0].BestNode)
	}
	if len(groups[0].Members) != 3 {
		t.Errorf("members: want 3, got %d", len(groups[0].Members))
	}
}

// A genuine split-brain (two cluster UUIDs) yields >1 group — the exact signal the
// belly-up authority gate refuses without --force. Majority lineage sorts first
// even when the minority has a higher seqno.
func TestBuildLineageGroups_SplitBrainMajorityFirst(t *testing.T) {
	rec := map[string]wsrepRecoverResult{
		"n0": {UUID: "aaa", Seqno: 552461, LastCommitted: 552461, Valid: true},
		"n1": {UUID: "aaa", Seqno: 552481, LastCommitted: 552481, Valid: true},
		"n2": {UUID: "bbb", Seqno: 552999, LastCommitted: 552999, Valid: true}, // higher seqno, but minority
	}
	groups := buildLineageGroups(rec)
	if len(groups) != 2 {
		t.Fatalf("split-brain: want 2 lineage groups, got %d", len(groups))
	}
	if groups[0].UUID != "aaa" || len(groups[0].Members) != 2 {
		t.Errorf("groups[0] must be majority lineage aaa (2 members), got %s (%d members)", groups[0].UUID, len(groups[0].Members))
	}
	if groups[0].BestNode != "n1" {
		t.Errorf("majority bestNode: want n1, got %s", groups[0].BestNode)
	}
}

// Nodes that never joined (zero UUID) or report a phantom/corrupt seqno are
// excluded, so they can't fabricate a lineage or win authority.
func TestBuildLineageGroups_ExcludesZeroUUIDAndPhantom(t *testing.T) {
	rec := map[string]wsrepRecoverResult{
		"real":    {UUID: "aaa", Seqno: 100, LastCommitted: 100, Valid: true},
		"never":   {UUID: zeroUUID, Seqno: 999, LastCommitted: 999, Valid: true},
		"phantom": {UUID: "bbb", Seqno: maxPhantomSeqno + 1, LastCommitted: 0, Valid: true},
	}
	groups := buildLineageGroups(rec)
	if len(groups) != 1 || groups[0].UUID != "aaa" {
		t.Fatalf("want only the real lineage aaa, got %+v", groups)
	}
}
