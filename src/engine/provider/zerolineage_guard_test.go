package provider

import (
	"errors"
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

// Committed data on the zero UUID is reported as ErrZeroLineage, at any seqno >= 0;
// a never-joined node (zero:-1) is still rejected, but is not lineage evidence.
func TestParseWsrepRecoverOutput_ZeroLineageIsEvidence(t *testing.T) {
	for _, seqno := range []int64{0, 15, 203767} {
		_, err := ParseWsrepRecoverOutput(fmt.Sprintf("Recovered position: %s:%d\n", ZeroUUID, seqno))
		if !errors.Is(err, ErrZeroLineage) {
			t.Errorf("zero UUID at seqno %d: err %v; want ErrZeroLineage", seqno, err)
		}
	}
	_, err := ParseWsrepRecoverOutput("Recovered position: " + ZeroUUID + ":-1\n")
	if err == nil || errors.Is(err, ErrZeroLineage) {
		t.Fatalf("never-joined zero:-1: err %v; want a rejection that is not ErrZeroLineage", err)
	}
}

func TestIsLineageUUID(t *testing.T) {
	for _, u := range []string{"", "unknown", ZeroUUID} {
		if IsLineageUUID(u) {
			t.Errorf("IsLineageUUID(%q) = true; want false", u)
		}
	}
	if !IsLineageUUID("4385a998-d4b2-11f1-b872-5a19d1251c10") {
		t.Fatal("control: a real cluster UUID is a lineage")
	}
}
