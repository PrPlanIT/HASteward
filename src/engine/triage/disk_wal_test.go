package triage

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/output/model"
)

// TestWalDominant pins the "sus the WAL, don't expand" threshold. The verdict must be
// "drain WAL in place" ONLY when the WAL is a meaningful share of the volume AND draining
// it would actually bring the disk comfortably below full — not merely "some WAL exists".
func TestWalDominant(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name string
		ds   *model.DiskStats
		want bool
	}{
		{
			// Real incident: 8Gi volume, 8.25GB un-recycled WAL, 85MB data — draining frees ~99%.
			name: "wal_bloat_frees_the_disk",
			ds:   &model.DiskStats{TotalBytes: 8590000000, UsedBytes: 8339935232, WALBytes: 8254631936, DataBytes: 85295104},
			want: true,
		},
		{
			// The trap: 95% full but only ~5% WAL, ~90% real data. Clearing WAL frees nothing
			// meaningful — expansion is the honest answer, NOT a WAL drain.
			name: "full_of_data_tiny_wal",
			ds:   &model.DiskStats{TotalBytes: 8 * gib, UsedBytes: 76 * gib / 10, WALBytes: 4 * gib / 10, DataBytes: 72 * gib / 10},
			want: false,
		},
		{
			// Half WAL / half data at full — draining WAL takes it to 50%.
			name: "half_and_half",
			ds:   &model.DiskStats{TotalBytes: 8 * gib, UsedBytes: 8 * gib, WALBytes: 4 * gib, DataBytes: 4 * gib},
			want: true,
		},
		{
			// WAL is a meaningful 30% and data lands at 68% after drain (<= 70% ceiling) -> drain.
			name: "wal30_data68_under_ceiling",
			ds:   &model.DiskStats{TotalBytes: 100, UsedBytes: 98, WALBytes: 30, DataBytes: 68},
			want: true,
		},
		{
			// WAL meaningful but data at 72% after drain (> 70% ceiling) — volume genuinely tight.
			name: "wal25_data72_over_ceiling",
			ds:   &model.DiskStats{TotalBytes: 100, UsedBytes: 97, WALBytes: 25, DataBytes: 72},
			want: false,
		},
		{
			// WAL share below the 25% floor — not enough to sus the WAL even with room to spare.
			name: "wal_share_below_floor",
			ds:   &model.DiskStats{TotalBytes: 100, UsedBytes: 95, WALBytes: 15, DataBytes: 80},
			want: false,
		},
		{name: "nil_stats", ds: nil, want: false},
		{name: "capacity_only_no_breakdown", ds: &model.DiskStats{TotalBytes: 8 * gib}, want: false},
	}
	for _, c := range cases {
		if got := walDominant(c.ds); got != c.want {
			t.Errorf("%s: walDominant = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSuggestExpansion pins the expansion sizer: it lands data at ~targetPct, rounds up to a
// whole GiB, always grows, and reports the growth factor — and declines nonsense inputs.
func TestSuggestExpansion(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name       string
		ds         *model.DiskStats
		target     int
		wantGiB    int64 // expected new size in GiB; 0 => expect no suggestion
		wantFactor float64
	}{
		{ // 7Gi data on 8Gi, target 60% -> 7/0.6 = 11.67 -> round up 12Gi (1.5x)
			name: "data7_target60", ds: &model.DiskStats{TotalBytes: 8 * gib, DataBytes: 7 * gib},
			target: 60, wantGiB: 12, wantFactor: 1.5,
		},
		{ // 6Gi data (75% full) target 60% -> exactly 10Gi (1.25x)
			name: "data6_target60", ds: &model.DiskStats{TotalBytes: 8 * gib, DataBytes: 6 * gib},
			target: 60, wantGiB: 10, wantFactor: 1.25,
		},
		{name: "nil", ds: nil, target: 60, wantGiB: 0},
		{name: "no_data_breakdown", ds: &model.DiskStats{TotalBytes: 8 * gib}, target: 60, wantGiB: 0},
		{name: "target_zero", ds: &model.DiskStats{TotalBytes: 8 * gib, DataBytes: 6 * gib}, target: 0, wantGiB: 0},
		{name: "target_100", ds: &model.DiskStats{TotalBytes: 8 * gib, DataBytes: 6 * gib}, target: 100, wantGiB: 0},
	}
	for _, c := range cases {
		nb, factor := suggestExpansion(c.ds, c.target)
		if c.wantGiB == 0 {
			if nb != 0 {
				t.Errorf("%s: expected no suggestion, got %d bytes", c.name, nb)
			}
			continue
		}
		if nb != c.wantGiB*gib {
			t.Errorf("%s: newBytes = %dGiB, want %dGiB", c.name, nb/gib, c.wantGiB)
		}
		if factor < c.wantFactor-0.01 || factor > c.wantFactor+0.01 {
			t.Errorf("%s: factor = %.3f, want ~%.2f", c.name, factor, c.wantFactor)
		}
	}
}
