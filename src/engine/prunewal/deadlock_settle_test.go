package prunewal

import "testing"

// TestClassifyPrimarySettle pins the decision that turns a hand-off-and-hope into an
// owned outcome: after a PRIMARY deadlock-recovery, what does settlePrimary do given the
// cluster's live currentPrimary/targetPrimary? The recovered authority here is "nc-2".
func TestClassifyPrimarySettle(t *testing.T) {
	const authority = "nc-2"
	cases := []struct {
		name           string
		currentPrimary string
		targetPrimary  string
		want           settleAction
	}{
		{
			// The live incident: authority is still the current primary, but a failover it
			// can never finish is open against nc-1. This is the case the feature exists for.
			name: "stuck_failover_against_authority", currentPrimary: "nc-2", targetPrimary: "nc-1", want: settleCancel,
		},
		{
			// Aligned: no failover in flight — just wait for the pod to be recreated + Ready.
			name: "aligned_wait_for_pod", currentPrimary: "nc-2", targetPrimary: "nc-2", want: settleWait,
		},
		{
			// targetPrimary not yet reported — no failover to cancel, keep watching.
			name: "empty_target_wait", currentPrimary: "nc-2", targetPrimary: "", want: settleWait,
		},
		{
			// The dangerous, DIFFERENT case: the failover actually completed to nc-1, so the
			// authority is no longer primary. Realigning would be wrong — escalate to promote.
			name: "failover_completed_elsewhere", currentPrimary: "nc-1", targetPrimary: "nc-1", want: settleCompletedElsewhere,
		},
		{
			// Completed-elsewhere dominates even mid-transition (current already moved off the
			// authority while target still trails).
			name: "moved_off_authority_target_trailing", currentPrimary: "nc-1", targetPrimary: "nc-2", want: settleCompletedElsewhere,
		},
		{
			// currentPrimary blank is transient (operator hasn't re-reported), NOT a completed
			// failover — must not be misread as "promote elsewhere".
			name: "blank_current_is_transient", currentPrimary: "", targetPrimary: "nc-1", want: settleWait,
		},
	}
	for _, c := range cases {
		if got := classifyPrimarySettle(authority, c.currentPrimary, c.targetPrimary); got != c.want {
			t.Errorf("%s: classifyPrimarySettle(%q,%q,%q) = %d, want %d",
				c.name, authority, c.currentPrimary, c.targetPrimary, got, c.want)
		}
	}
}
