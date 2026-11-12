package provider

import (
	"fmt"
	"testing"
)

// Every zero-UUID guard compares against this constant; it stays the all-zero identity.
func TestZeroUUIDIsTheAllZeroIdentity(t *testing.T) {
	if ZeroUUID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("ZeroUUID = %q; it must be the all-zero cluster UUID", ZeroUUID)
	}
}

// A recovered zero UUID is rejected at any seqno, never only at small ones.
func TestParseWsrepRecoverOutput_ZeroUUIDRejectedAtAnySeqno(t *testing.T) {
	for _, seqno := range []int64{1, 15, 123702, 203767} {
		out := fmt.Sprintf("Recovered position: %s:%d\n", ZeroUUID, seqno)
		if _, err := ParseWsrepRecoverOutput(out); err == nil {
			t.Errorf("zero UUID at seqno %d parsed as a position; it must be rejected", seqno)
		}
	}
}
