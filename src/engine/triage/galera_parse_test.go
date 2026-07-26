package triage

import (
	"context"
	"testing"
)

// TestParseRecoveredPositionFromLog: the parse that un-blinds a wedged node. A wrong
// parse here returns a too-high seqno that out-ranks the true authority; a missed
// parse leaves it Unread (fail-closed, safe).
func TestParseRecoveredPositionFromLog(t *testing.T) {
	t.Run("explicit recovered position", func(t *testing.T) {
		s, u := parseRecoveredPositionFromLog("2026 [Note] WSREP: Recovered position: " + recoverUUID2 + ":128\n")
		if s != 128 || u != recoverUUID2 {
			t.Fatalf("got (%d, %q)", s, u)
		}
	})

	t.Run("gcache gapless sequence upper bound", func(t *testing.T) {
		s, u := parseRecoveredPositionFromLog("Recovering GCache ring buffer: found gapless sequence 90-137\n")
		if s != 137 || u != "" {
			t.Fatalf("want (137, \"\") from the gapless upper bound, got (%d, %q)", s, u)
		}
	})

	t.Run("max across lines", func(t *testing.T) {
		log := "found gapless sequence 10-20\nWSREP: Recovered position: " + recoverUUID2 + ":50\nfound gapless sequence 40-45\n"
		s, _ := parseRecoveredPositionFromLog(log)
		if s != 50 {
			t.Fatalf("want the max (50), got %d", s)
		}
	})

	t.Run("neither present", func(t *testing.T) {
		s, u := parseRecoveredPositionFromLog("just some boot logs\nnothing useful\n")
		if s != -1 || u != "" {
			t.Fatalf("want (-1, \"\"), got (%d, %q)", s, u)
		}
	})
}

// TestAnyServing_EmptyPasswordFailsSafe pins the fence-gate safety net: with no root
// password we cannot authenticate to prove the cluster is dead, so anyServing must
// return true (assume serving) — so maybeDeepRecover never fences a cluster it can't
// prove is belly-up. NewGaleraProviderForTest leaves the root password empty.
func TestAnyServing_EmptyPasswordFailsSafe(t *testing.T) {
	tr := galeraTriageForTest()
	if !tr.anyServing(context.Background(), &galeraTriageData{}) {
		t.Fatal("with an empty root password, anyServing must assume the cluster is serving (never fence blind)")
	}
}

const recoverUUID2 = "57a2c75c-6dea-11f1-b59b-266ed2cb955a"
