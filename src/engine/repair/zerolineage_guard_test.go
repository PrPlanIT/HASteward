package repair

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/engine/provider"
)

// A donor on no cluster lineage is refused: an SST from it carries no position, so
// the joiner is wiped and never joins.
func TestDonorLineageError_ZeroUUIDDonorRefused(t *testing.T) {
	for _, u := range []string{provider.ZeroUUID, "", "unknown"} {
		if err := donorLineageError("c-1", DonorProbeResult{ExecOK: true, StateComment: "Synced", ClusterUUID: u}); err == nil {
			t.Errorf("donor with cluster_state_uuid %q accepted; want refused", u)
		}
	}
	if err := donorLineageError("c-1", DonorProbeResult{ExecOK: true, StateComment: "Synced",
		ClusterUUID: "4385a998-d4b2-11f1-b872-5a19d1251c10"}); err != nil {
		t.Fatalf("control: a real-lineage donor was refused: %v", err)
	}
}
