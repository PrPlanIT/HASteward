package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// TestClassifyInstance pins the fail-closed behaviour: anything not provably
// disposable (unreadable, split-brain, unknown timeline) must be Unknown.
func TestClassifyInstance(t *testing.T) {
	tests := []struct {
		name                                        string
		isPrimary, isAuthority, hasData, safeToHeal bool
		behindTL, sameTL, stranded                  bool
		want                                        model.Classification
	}{
		{"unreadable -> unknown (fail closed)", false, false, false, true, true, false, false, model.ClassUnknown},
		{"split-brain -> unknown (fail closed)", false, false, true, false, true, false, false, model.ClassUnknown},
		{"primary -> authoritative", true, true, true, true, false, false, false, model.ClassAuthoritative},
		{"most-advanced non-primary -> authoritative", false, true, true, true, false, true, false, model.ClassAuthoritative},
		{"behind timeline -> disposable", false, false, true, true, true, false, false, model.ClassDisposable},
		{"same timeline, streaming -> recoverable", false, false, true, true, false, true, false, model.ClassRecoverable},
		{"same timeline but stranded (WAL recycled) -> disposable", false, false, true, true, false, true, true, model.ClassDisposable},
		{"unknown timeline -> unknown (fail closed)", false, false, true, true, false, false, false, model.ClassUnknown},
		// a behind-timeline instance is NEVER disposable if it's somehow the authority
		{"authority wins over behind-timeline", false, true, true, true, true, false, false, model.ClassAuthoritative},
		// stranded likewise never overrides authority
		{"authority wins over stranded", false, true, true, true, false, true, true, model.ClassAuthoritative},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyInstance(tt.isPrimary, tt.isAuthority, tt.hasData, tt.safeToHeal, tt.behindTL, tt.sameTL, tt.stranded)
			if got != tt.want {
				t.Errorf("classifyInstance() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveRecovery(t *testing.T) {
	safe := model.DataComparison{SafeToHeal: true}
	unsafe := model.DataComparison{SafeToHeal: false}

	// Healthy cluster: nothing disposable -> nil (no recovery block).
	healthy := []model.InstanceAssessment{
		{Pod: "pg-1", Classification: model.ClassAuthoritative},
		{Pod: "pg-2", Classification: model.ClassRecoverable},
	}
	if r := deriveRecovery(healthy, safe, "pg-1", "Cluster in healthy state", true); r != nil {
		t.Errorf("healthy: expected nil recovery, got %+v", r)
	}

	// Ambiguous authority (split-brain): refuse, never blocked.
	r := deriveRecovery(healthy, unsafe, "pg-1", "x", true)
	if r == nil || r.Reason != "ambiguous_authority" || r.Blocked {
		t.Errorf("ambiguous: expected refuse, got %+v", r)
	}

	// Blocked deadlock (the calcom shape): disposable disk-full, primary down, frozen.
	deadlock := []model.InstanceAssessment{
		{Pod: "pg-3", Classification: model.ClassAuthoritative},
		{Pod: "pg-1", Classification: model.ClassDisposable, Notes: []string{"behind: timeline 16 < primary 17", "disk full (WAL accumulation from being stuck)"}},
		{Pod: "pg-2", Classification: model.ClassDisposable, Notes: []string{"behind: timeline 16 < primary 17"}},
	}
	r = deriveRecovery(deadlock, safe, "pg-3", "Not enough disk space", false)
	if r == nil {
		t.Fatal("deadlock: expected recovery, got nil")
	}
	if !r.Blocked || r.Reason != "disk_full_disposable_replica" {
		t.Errorf("deadlock: blocked=%v reason=%q, want true/disk_full_disposable_replica", r.Blocked, r.Reason)
	}
	if r.Authority != "pg-3" {
		t.Errorf("deadlock: authority = %q, want pg-3", r.Authority)
	}
	// Authority is down, so the recovery set must include it first, then the disposables.
	if got, want := r.RecoverySet, []string{"pg-3", "pg-1", "pg-2"}; !equalStrs(got, want) {
		t.Errorf("deadlock: recoverySet = %v, want %v", got, want)
	}

	// Same disposable set but authority UP and not frozen -> not blocked, recovery set excludes authority.
	r = deriveRecovery(deadlock, safe, "pg-3", "Cluster in healthy state", true)
	if r == nil || r.Blocked {
		t.Errorf("authority-up: expected unblocked recovery, got %+v", r)
	}
	if got, want := r.RecoverySet, []string{"pg-1", "pg-2"}; !equalStrs(got, want) {
		t.Errorf("authority-up: recoverySet = %v, want %v", got, want)
	}
}

func TestParseDiskStats(t *testing.T) {
	raw := `Latest checkpoint's TimeLineID: 17
===DF===
/dev/rbd1 10485760 9437184 1048576 90% /var/lib/postgresql/data
===WAL===
655360 /var/lib/postgresql/data/pgdata/pg_wal
===PGDATA===
702464 /var/lib/postgresql/data/pgdata
===SEGMENTS===
40`
	ds := parseDiskStats(raw, "pvc_probe")
	if ds.Source != "pvc_probe" {
		t.Errorf("source = %q", ds.Source)
	}
	if ds.TotalBytes != 10485760*1024 {
		t.Errorf("total = %d, want %d", ds.TotalBytes, int64(10485760*1024))
	}
	if ds.UsedPercent != 90 {
		t.Errorf("usedPercent = %d, want 90", ds.UsedPercent)
	}
	if ds.WALBytes != 655360*1024 {
		t.Errorf("wal = %d", ds.WALBytes)
	}
	if want := int64(702464-655360) * 1024; ds.DataBytes != want {
		t.Errorf("data = %d, want %d (pgdata - wal)", ds.DataBytes, want)
	}
	if ds.WALSegments != 40 {
		t.Errorf("segments = %d, want 40", ds.WALSegments)
	}

	// Empty / unreadable input still yields an explicit Source, never a panic.
	if got := parseDiskStats("", "none"); got == nil || got.Source != "none" || got.TotalBytes != 0 {
		t.Errorf("empty input: got %+v", got)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
