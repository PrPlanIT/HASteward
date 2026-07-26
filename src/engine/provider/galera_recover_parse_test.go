package provider

import "testing"

const recoverUUID = "57a2c75c-6dea-11f1-b59b-266ed2cb955a"

func TestParseWsrepRecoverOutput(t *testing.T) {
	t.Run("valid with last-committed", func(t *testing.T) {
		out := "WSREP: Recovered position: " + recoverUUID + ":42\nLast committed: 40\n"
		rr, err := ParseWsrepRecoverOutput(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !rr.Valid || rr.UUID != recoverUUID || rr.Seqno != 42 || rr.LastCommitted != 40 {
			t.Fatalf("bad parse: %+v", rr)
		}
	})

	t.Run("last-committed falls back to seqno when absent", func(t *testing.T) {
		rr, err := ParseWsrepRecoverOutput("Recovered position: " + recoverUUID + ":42\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rr.LastCommitted != 42 {
			t.Fatalf("LastCommitted should fall back to seqno 42, got %d", rr.LastCommitted)
		}
	})

	t.Run("zero UUID rejected (never joined)", func(t *testing.T) {
		if _, err := ParseWsrepRecoverOutput("Recovered position: " + ZeroUUID + ":5\n"); err == nil {
			t.Fatal("a zero UUID must be rejected")
		}
	})

	t.Run("negative seqno rejected", func(t *testing.T) {
		if _, err := ParseWsrepRecoverOutput("Recovered position: " + recoverUUID + ":-1\n"); err == nil {
			t.Fatal("a negative seqno must be rejected")
		}
	})

	t.Run("phantom seqno rejected", func(t *testing.T) {
		out := "Recovered position: " + recoverUUID + ":9999999999999\n" // > MaxPhantomSeqno (1e12)
		if _, err := ParseWsrepRecoverOutput(out); err == nil {
			t.Fatal("a seqno above MaxPhantomSeqno must be rejected")
		}
	})

	t.Run("no recovered-position line", func(t *testing.T) {
		rr, err := ParseWsrepRecoverOutput("mariadbd starting...\nsome other log\n")
		if err == nil {
			t.Fatal("missing recovered position must error")
		}
		if rr.Valid {
			t.Fatal("Valid must be false on a failed parse")
		}
	})
}
