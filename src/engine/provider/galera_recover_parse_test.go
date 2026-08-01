package provider

import (
	"strings"
	"testing"
)

const recoverUUID = "57a2c75c-6dea-11f1-b59b-266ed2cb955a"

// TestWsrepRecoverCommand_ForcesRowBinlog guards the live-found bug: without
// --binlog-format=ROW, mariadbd --wsrep-recover aborts on modern MariaDB (default MIXED)
// with "Only binlog_format='ROW' is currently supported" and never prints a position, so
// every node's recovery parse fails and a belly-up bootstrap dies with "candidate pod not
// found". This flag must always be present.
func TestWsrepRecoverCommand_ForcesRowBinlog(t *testing.T) {
	cmd := wsrepRecoverCommand("/usr/lib/galera/libgalera_smm.so")
	if !strings.Contains(cmd, "--binlog-format=ROW") {
		t.Fatalf("wsrep_recover command MUST force --binlog-format=ROW, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--wsrep-recover") || !strings.Contains(cmd, "libgalera_smm.so") {
		t.Fatalf("wsrep_recover command malformed: %s", cmd)
	}
}

// TestParseWsrepRecoverOutput_MariaDB118 parses the REAL MariaDB 11.8.8 recovery line
// (the format captured live) so the parser can't regress against it.
func TestParseWsrepRecoverOutput_MariaDB118(t *testing.T) {
	out := "2026-08-01  0:57:29 0 [Note] WSREP: Recovered position: 87eed17c-80e4-11f1-9ce5-3e85072fd6e9:54784\n"
	rr, err := ParseWsrepRecoverOutput(out)
	if err != nil {
		t.Fatalf("must parse MariaDB 11.8.8 output: %v", err)
	}
	if !rr.Valid || rr.UUID != "87eed17c-80e4-11f1-9ce5-3e85072fd6e9" || rr.Seqno != 54784 {
		t.Fatalf("bad parse of 11.8.8 output: %+v", rr)
	}
}

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
