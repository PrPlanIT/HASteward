package triage

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestDeriveEffectiveSeqno pins bug #4: the operator's galeraRecovery snapshot
// (cr_recovered/cr_state) and the GCache estimate are HINTS ONLY — they may never
// establish an authoritative (Known) position or drive the bootstrap decision.
// Only a live wsrep_last_committed, a fresh --wsrep-recover, or a clean grastate
// (>0) is authoritative. This is the fix that stops us trusting the fake 3292 the
// operator reported when --wsrep-recover proved every node at 552481.
func TestDeriveEffectiveSeqno(t *testing.T) {
	const none = int64(-1)
	tests := []struct {
		name                                              string
		wsCommitted, crRec, crState, grastate, log, recov int64
		wantValue                                         int64
		wantSource                                        string
		wantKnown                                         bool
	}{
		{"cr_recovered alone is NOT authoritative (hint only)",
			none, 3292, none, none, none, none, 3292, "cr_recovered", false},
		{"cr_state alone is NOT authoritative (hint only)",
			none, none, 3289, none, none, none, 3289, "cr_state", false},
		{"gcache estimate alone is NOT authoritative (hint only)",
			none, none, none, none, 552461, none, 552461, "log_gcache_estimate", false},
		{"live wsrep_last_committed is authoritative",
			552481, none, none, none, none, none, 552481, "wsrep_last_committed", true},
		{"clean grastate (>0) is authoritative",
			none, none, none, 552481, none, none, 552481, "grastate", true},
		{"unclean grastate (-1) is not usable -> unknown",
			none, none, none, none, none, none, none, "none", false},
		{"fresh wsrep_recover wins over the fake cr_recovered and the gcache estimate",
			none, 3292, none, none, 552461, 552481, 552481, "wsrep_recover", true},
		{"osticket reality: cr says 3292/3289, recover proves 552481 -> 552481 KNOWN",
			none, 3292, 3289, none, 552461, 552481, 552481, "wsrep_recover", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := deriveEffectiveSeqno(tt.wsCommitted, tt.crRec, tt.crState, tt.grastate, tt.log, tt.recov)
			if es.Value != tt.wantValue || es.Source != tt.wantSource || es.Known != tt.wantKnown {
				t.Fatalf("got {value=%d source=%q known=%v}, want {value=%d source=%q known=%v}",
					es.Value, es.Source, es.Known, tt.wantValue, tt.wantSource, tt.wantKnown)
			}
		})
	}
}

// TestIsAnythingAlive pins bug #5's fail-SAFE gate: triage may escalate to the
// fenced recovery ONLY when nothing is live. Any Primary member, Ready mariadb
// container, or wsrep participation (local_state>=1) means hands-off.
func TestIsAnythingAlive(t *testing.T) {
	readyMariadb := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "mariadb", Ready: true}}}}
	unreadyMariadb := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "mariadb", Ready: false}}}}
	tests := []struct {
		name string
		data *galeraTriageData
		want bool
	}{
		{"primary component exists -> alive",
			&galeraTriageData{primaryMembers: []string{"n-0"}}, true},
		{"synced wsrep (state 4) -> alive",
			&galeraTriageData{wsrepMap: map[string]*wsrepStatus{"n-0": {LocalState: 4}}}, true},
		{"joining wsrep (state 1) -> alive (progressing; don't interrupt)",
			&galeraTriageData{wsrepMap: map[string]*wsrepStatus{"n-0": {LocalState: 1}}}, true},
		{"ready mariadb container -> alive even if the live query hiccupped",
			&galeraTriageData{runningPods: []corev1.Pod{readyMariadb}}, true},
		{"all non-primary, unready, no wsrep participation -> PROVABLY DEAD (osticket)",
			&galeraTriageData{
				runningPods: []corev1.Pod{unreadyMariadb},
				wsrepMap:    map[string]*wsrepStatus{"n-0": {LocalState: 0, ClusterStatus: "non-Primary"}},
			}, false},
		{"empty (all pods gone) -> provably dead",
			&galeraTriageData{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAnythingAlive(tt.data); got != tt.want {
				t.Fatalf("isAnythingAlive = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCrossInstanceComparison_FailClosed pins bug #1: a node whose seqno is not
// authoritatively Known makes SafeToHeal false and authority undeterminable — the
// tool must never declare a bootstrap target while blind to a node.
func TestCrossInstanceComparison_FailClosed(t *testing.T) {
	tr := &galeraTriage{}

	t.Run("unread node -> NOT safe, explicitly flagged", func(t *testing.T) {
		data := &galeraTriageData{
			grastateData: []grastate{{Pod: "n-0", UUID: "u1"}, {Pod: "n-1", UUID: "u1"}},
			effectiveSeqnos: map[string]*effectiveSeqno{
				"n-0": {Value: 552481, Source: "wsrep_recover", Known: true},
				"n-1": {Value: -1, Source: "none", Known: false}, // UNREAD
			},
			wsrepMap: map[string]*wsrepStatus{},
		}
		cmp := tr.crossInstanceComparison(data)
		if cmp.SafeToHeal {
			t.Fatalf("SafeToHeal=true with an unread node; want false (fail-closed)")
		}
		if !containsSub(cmp.SplitBrainDetails, "UNREAD") {
			t.Fatalf("expected an UNREAD SEQNO flag, got %v", cmp.SplitBrainDetails)
		}
	})

	t.Run("all authoritative + same uuid -> safe", func(t *testing.T) {
		data := &galeraTriageData{
			grastateData: []grastate{{Pod: "n-0", UUID: "u1"}, {Pod: "n-1", UUID: "u1"}, {Pod: "n-2", UUID: "u1"}},
			effectiveSeqnos: map[string]*effectiveSeqno{
				"n-0": {Value: 552481, Source: "wsrep_recover", Known: true},
				"n-1": {Value: 552481, Source: "wsrep_recover", Known: true},
				"n-2": {Value: 552481, Source: "wsrep_recover", Known: true},
			},
			wsrepMap: map[string]*wsrepStatus{},
		}
		cmp := tr.crossInstanceComparison(data)
		if !cmp.SafeToHeal {
			t.Fatalf("SafeToHeal=false with all nodes authoritative + identical; want true. details=%v", cmp.SplitBrainDetails)
		}
	})

	t.Run("divergent uuid -> NOT safe (split-brain)", func(t *testing.T) {
		data := &galeraTriageData{
			grastateData: []grastate{{Pod: "n-0", UUID: "u1"}, {Pod: "n-1", UUID: "u2"}},
			effectiveSeqnos: map[string]*effectiveSeqno{
				"n-0": {Value: 100, Source: "wsrep_recover", Known: true},
				"n-1": {Value: 200, Source: "wsrep_recover", Known: true},
			},
			wsrepMap: map[string]*wsrepStatus{},
		}
		cmp := tr.crossInstanceComparison(data)
		if cmp.SafeToHeal {
			t.Fatalf("SafeToHeal=true with divergent UUIDs; want false")
		}
	})
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
