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
	ds := tr.diagnose(cmp, nil, "pg-1", &cnpgTriageData{})
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
	d := tr.diagnose(cmp, assess, "pg-1", &cnpgTriageData{})[0]
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
	ds := tr.diagnose(cmp, nil, "pg-1", &cnpgTriageData{})
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
	if ds := tr.diagnose(model.DataComparison{SafeToHeal: true, Authority: model.AuthorityProvable}, nil, "pg-1", &cnpgTriageData{}); ds != nil {
		t.Fatalf("safe heal must yield no diagnosis, got %+v", ds)
	}
	if ds := tr.diagnose(model.DataComparison{SafeToHeal: false, Authority: model.AuthorityUndeterminable}, nil, "pg-1", &cnpgTriageData{}); ds != nil {
		t.Fatalf("undeterminable is handled by the banner (bring up for inspection), not a rebuild diagnosis, got %+v", ds)
	}
}

// TestDiagnoseTimelineRewind is the P3.5 signal: a backwards fork (a restore/PITR to an
// earlier LSN) must be flagged, while a healthy monotonic history (normal failovers) is
// silent. Uses the boundary-postgres fork sequence — monotonic until TL9 forks at
// 2C/99, behind TL8's 8E/EE.
func TestDiagnoseTimelineRewind(t *testing.T) {
	rewound := histFiles("11/840000A0", "2C/960013E8", "72/D8007388", "78/59032EF0",
		"8E/9E0000A0", "8E/E40030C0", "8E/EE029E80", "2C/99000000") // TL9, last fork goes backwards
	healthy := histFiles("10/00000000", "20/00000000", "30/00000000") // TL4, strictly forward

	// Rewind present on the TL9 instance → flagged, headline names it and both LSNs.
	data := &cnpgTriageData{controlData: []controlData{
		{Pod: "boundary-postgres-2", Timeline: "8", HistoryRaw: healthy},
		{Pod: "boundary-postgres-3", Timeline: "9", HistoryRaw: rewound},
	}}
	d := diagnoseTimelineRewind(data, "boundary-postgres", "hookshot")
	if d == nil {
		t.Fatal("a backwards fork must be flagged as a rewind")
	}
	if d.Target != "boundary-postgres-3" || !strings.Contains(d.Summary, "timeline 9") {
		t.Fatalf("rewind must headline the TL9 instance, got target=%q summary=%q", d.Target, d.Summary)
	}
	if !strings.Contains(d.Summary, "2C/99000000") || !strings.Contains(d.Summary, "8E/EE029E80") {
		t.Fatalf("summary must show the backwards fork LSN and the earlier fork it undercut: %q", d.Summary)
	}

	// A cluster with only monotonic histories → no rewind diagnosis.
	clean := &cnpgTriageData{controlData: []controlData{
		{Pod: "pg-1", Timeline: "4", HistoryRaw: healthy},
	}}
	if d := diagnoseTimelineRewind(clean, "pg", "ns"); d != nil {
		t.Fatalf("healthy forward-only history must not be flagged, got %+v", d)
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
