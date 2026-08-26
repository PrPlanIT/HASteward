package prunewal

import "testing"

// TestSafeToReleaseRecoveredPrimary pins the release-vs-guard gate that replaced the old
// unconditional force-back: hand the recovered primary back ONLY when a replica is provably
// caught up (same timeline, LSN >= the recovered checkpoint); otherwise keep it fenced so a
// behind or diverged replica can't be promoted over the authority.
func TestSafeToReleaseRecoveredPrimary(t *testing.T) {
	rec := func(tl int64, lsn uint64) instancePos { return instancePos{pod: "pg-1", timeline: tl, lsn: lsn} }
	rep := func(pod string, tl int64, lsn uint64) instancePos { return instancePos{pod: pod, timeline: tl, lsn: lsn} }

	cases := []struct {
		name      string
		recovered instancePos
		replicas  []instancePos
		want      bool
	}{
		{
			// The nextcloud case: replicas caught up on the same timeline → releasing is safe
			// (CNPG can promote either and lose nothing). This is what the old code wrongly fought.
			name: "replica_caught_up_same_timeline", recovered: rec(30, 0xC4CD000028),
			replicas: []instancePos{rep("pg-2", 30, 0xC4DD000000), rep("pg-3", 30, 0xC4CD000028)}, want: true,
		},
		{
			// Replica exactly at the recovered checkpoint (>= is inclusive) → safe.
			name: "replica_exactly_equal", recovered: rec(30, 0xC4CD000028),
			replicas: []instancePos{rep("pg-2", 30, 0xC4CD000028)}, want: true,
		},
		{
			// The data-loss case: every replica is BEHIND on the same timeline (the disk-full
			// primary held commits they never streamed) → guard, do not release.
			name: "all_replicas_behind_same_timeline", recovered: rec(30, 0xC4CD000028),
			replicas: []instancePos{rep("pg-2", 30, 0xC4CB000000), rep("pg-3", 30, 0xC4C0000000)}, want: false,
		},
		{
			// A replica already promoted to a forked timeline. Even with a numerically-higher
			// LSN it is a DIVERGENCE, not "caught up" — guard (this is the split-brain shape).
			name: "replica_on_forked_timeline", recovered: rec(30, 0xC4CD000028),
			replicas: []instancePos{rep("pg-2", 31, 0xC4FF000000)}, want: false,
		},
		{
			// Mixed: one behind on-timeline, one ahead but forked → still no provably-caught-up
			// same-timeline replica → guard.
			name: "mixed_behind_and_forked", recovered: rec(30, 0xC4CD000028),
			replicas: []instancePos{rep("pg-2", 30, 0xC4C0000000), rep("pg-3", 31, 0xC4FF000000)}, want: false,
		},
		{
			// No readable replica at all → cannot prove safety → guard.
			name: "no_replicas", recovered: rec(30, 0xC4CD000028), replicas: nil, want: false,
		},
	}
	for _, c := range cases {
		if got := safeToReleaseRecoveredPrimary(c.recovered, c.replicas); got != c.want {
			t.Errorf("%s: safeToReleaseRecoveredPrimary = %v, want %v", c.name, got, c.want)
		}
	}
}
