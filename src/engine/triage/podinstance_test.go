package triage

import "testing"

// InstanceAssessment.Instance was never set on the CNPG path, so it read 0 for every
// instance. runEscrow names each diverged lineage's escrow after it, which sent all
// of gitlab-postgresql's lineages to one path. 0 is a real ordinal, so the unset
// value was indistinguishable from instance 0 — hence -1 for "not derivable".
func TestPodInstanceNumber(t *testing.T) {
	for _, tc := range []struct {
		pod  string
		want int
	}{
		{"gitlab-postgresql-1", 1},
		{"gitlab-postgresql-2", 2},
		{"gitlab-postgresql-13", 13},
		{"c-0", 0},
		{"nextcloud-postgres-3", 3},
		{"", -1},
		{"no-trailing-ordinal", -1},
		{"trailing-dash-", -1},
	} {
		if got := podInstanceNumber(tc.pod); got != tc.want {
			t.Errorf("podInstanceNumber(%q) = %d, want %d", tc.pod, got, tc.want)
		}
	}
}
