package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

func TestAuthorityWarnings(t *testing.T) {
	// Safe → just the OK message.
	if got := authorityWarnings(true, "OK: all good", []string{"ignored"}); len(got) != 1 || got[0] != "OK: all good" {
		t.Fatalf("safe warnings wrong: %v", got)
	}
	// Safe with no OK message → nil.
	if got := authorityWarnings(true, "", nil); got != nil {
		t.Fatalf("empty okMsg should yield nil, got %v", got)
	}
	// Unsafe → every reason prefixed, OK message ignored.
	got := authorityWarnings(false, "OK: unused", []string{"reason A", "reason B"})
	want := []string{"SPLIT-BRAIN RISK: reason A", "SPLIT-BRAIN RISK: reason B"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unsafe warnings wrong: %v", got)
	}
}

func TestNewAuthorityComparison(t *testing.T) {
	// Provable → safe, OK warning, no split-brain noise.
	prov := newAuthorityComparison(model.AuthorityProvable, "pg-1", 9, nil, "OK: primary leads")
	if !prov.SafeToHeal || prov.Authority != model.AuthorityProvable {
		t.Fatalf("provable should be safe: %+v", prov)
	}
	if len(prov.Warnings) != 1 || prov.Warnings[0] != "OK: primary leads" {
		t.Fatalf("provable warnings wrong: %v", prov.Warnings)
	}
	if prov.MostAdvanced != "pg-1" || prov.MostAdvancedValue != 9 {
		t.Fatalf("most-advanced not carried: %+v", prov)
	}

	// Diverged → not safe; reasons become SplitBrainDetails and prefixed warnings.
	div := newAuthorityComparison(model.AuthorityDiverged, "", 0, []string{"two lineages"}, "OK: unused")
	if div.SafeToHeal {
		t.Fatal("diverged must not be safe")
	}
	if len(div.SplitBrainDetails) != 1 || div.SplitBrainDetails[0] != "two lineages" {
		t.Fatalf("split-brain details wrong: %v", div.SplitBrainDetails)
	}
	if len(div.Warnings) != 1 || div.Warnings[0] != "SPLIT-BRAIN RISK: two lineages" {
		t.Fatalf("diverged warnings wrong: %v", div.Warnings)
	}
}

func TestDeriveAuthorityStatus(t *testing.T) {
	if deriveAuthorityStatus(true) != "unambiguous" {
		t.Fatal("safe → unambiguous")
	}
	if deriveAuthorityStatus(false) != "ambiguous" {
		t.Fatal("unsafe → ambiguous")
	}
}
