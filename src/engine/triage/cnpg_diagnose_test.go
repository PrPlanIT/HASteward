package triage

import (
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// TestDiagnose_LeaderNotPrimary is the P3.2 core: when the authority is a replica and
// not the primary, triage must emit a NAMED recovery diagnosis targeting the authority
// with an ordered, escrow-first rebuild-around-authority plan — not just refuse.
func TestDiagnose_LeaderNotPrimary(t *testing.T) {
	tr := cnpgTriageForTest()
	cmp := model.DataComparison{
		SafeToHeal:   false,
		Authority:    model.AuthorityLeaderNotPrimary,
		MostAdvanced: "pg-2", // the authority replica
	}
	ds := tr.diagnose(cmp, nil, "pg-1")
	if len(ds) != 1 {
		t.Fatalf("want 1 diagnosis, got %d: %+v", len(ds), ds)
	}
	d := ds[0]
	if d.ID != "cnpg-authority-not-primary" {
		t.Fatalf("wrong id: %q", d.ID)
	}
	if d.Target != "pg-2" {
		t.Fatalf("remediation target must be the authority pg-2, got %q", d.Target)
	}
	// The plan must be escrow-first and end at the standard heal-from-primary, and must
	// name the promotion as the deliberate/manual step (not silently automate it).
	for _, want := range []string{"Escrow every instance", "hasteward repair -e cnpg", "does not yet automate"} {
		if !strings.Contains(d.Detail, want) {
			t.Fatalf("recovery plan missing %q; detail=\n%s", want, d.Detail)
		}
	}
	if !strings.Contains(d.Remedy, "hasteward backup") {
		t.Fatalf("first safe step should be an escrow (no disk pressure): %q", d.Remedy)
	}
}

// TestDiagnose_LeaderNotPrimary_DiskWedged: a disk-full authority must be relieved
// (prune wal --dry-run) FIRST, before it can be brought up.
func TestDiagnose_LeaderNotPrimary_DiskWedged(t *testing.T) {
	tr := cnpgTriageForTest()
	cmp := model.DataComparison{SafeToHeal: false, Authority: model.AuthorityLeaderNotPrimary, MostAdvanced: "pg-2"}
	assess := []model.InstanceAssessment{{Pod: "pg-2", CrashReason: "disk_full"}}
	d := tr.diagnose(cmp, assess, "pg-1")[0]
	if !strings.Contains(d.Remedy, "prune wal") || !strings.Contains(d.Remedy, "--instance 2") {
		t.Fatalf("wedged authority must be relieved first via prune wal --instance 2, got: %q", d.Remedy)
	}
	if !strings.Contains(d.Detail, "relieve WAL first") {
		t.Fatalf("plan must call out WAL relief for the wedged authority; detail=\n%s", d.Detail)
	}
}

// TestDiagnose_Diverged: no automatic winner — escrow-all diagnosis, no single target.
func TestDiagnose_Diverged(t *testing.T) {
	tr := cnpgTriageForTest()
	cmp := model.DataComparison{SafeToHeal: false, Authority: model.AuthorityDiverged}
	ds := tr.diagnose(cmp, nil, "pg-1")
	if len(ds) != 1 || ds[0].ID != "cnpg-split-brain-diverged" {
		t.Fatalf("want the diverged diagnosis, got %+v", ds)
	}
	if ds[0].Target != "" {
		t.Fatalf("divergence has no single authority — target must be empty, got %q", ds[0].Target)
	}
	if !strings.Contains(ds[0].Detail, "ESCROW every instance") {
		t.Fatalf("diverged plan must demand escrow-all first; detail=\n%s", ds[0].Detail)
	}
}

// TestDiagnose_SafeAndUndeterminable: a safe heal (or an undeterminable outcome, whose
// guidance is "bring the node up for inspection") produces no recovery diagnosis here.
func TestDiagnose_SafeAndUndeterminable(t *testing.T) {
	tr := cnpgTriageForTest()
	if ds := tr.diagnose(model.DataComparison{SafeToHeal: true, Authority: model.AuthorityProvable}, nil, "pg-1"); ds != nil {
		t.Fatalf("safe heal must yield no diagnosis, got %+v", ds)
	}
	if ds := tr.diagnose(model.DataComparison{SafeToHeal: false, Authority: model.AuthorityUndeterminable}, nil, "pg-1"); ds != nil {
		t.Fatalf("undeterminable is handled by the banner (bring up for inspection), not a rebuild diagnosis, got %+v", ds)
	}
}

func TestAuthorityIsDiskConstrained(t *testing.T) {
	as := []model.InstanceAssessment{
		{Pod: "pg-2", CrashReason: "disk_full"},
		{Pod: "pg-3", Disk: &model.DiskStats{TotalBytes: 100, UsedPercent: 96}},
		{Pod: "pg-4", Disk: &model.DiskStats{TotalBytes: 100, UsedPercent: 40}},
		{Pod: "pg-5"}, // unknown disk → must NOT fabricate relief
	}
	if !authorityIsDiskConstrained("pg-2", as) {
		t.Error("disk_full crash must count as constrained")
	}
	if !authorityIsDiskConstrained("pg-3", as) {
		t.Error("≥95% used must count as constrained")
	}
	if authorityIsDiskConstrained("pg-4", as) {
		t.Error("40% used is not constrained")
	}
	if authorityIsDiskConstrained("pg-5", as) {
		t.Error("unknown disk must be conservative (false), not fabricate relief")
	}
}
