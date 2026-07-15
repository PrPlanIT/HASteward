package triage

import "testing"

// TestParseWsrepStatus guards the map-based parse (fed by provider.QueryWsrep).
func TestParseWsrepStatus(t *testing.T) {
	ws := parseWsrepStatus(map[string]string{
		"wsrep_local_state":         "4",
		"wsrep_local_state_comment": "Synced",
		"wsrep_cluster_status":      "Primary",
		"wsrep_cluster_size":        "3",
		"wsrep_connected":           "ON",
		"wsrep_ready":               "ON",
		"wsrep_cluster_state_uuid":  "abc",
		"wsrep_last_committed":      "552481",
		"wsrep_flow_control_paused": "0.0",
	})
	if ws.LocalState != 4 || ws.LocalStateComment != "Synced" || ws.ClusterStatus != "Primary" ||
		ws.ClusterSize != "3" || ws.Connected != "ON" || ws.Ready != "ON" ||
		ws.ClusterStateUUID != "abc" || ws.LastCommitted != 552481 || ws.FlowControlPaused != "0.0" {
		t.Fatalf("parse mismatch: %+v", ws)
	}

	// Absent wsrep_last_committed must default to -1 (unknown), never 0.
	if empty := parseWsrepStatus(map[string]string{}); empty.LastCommitted != -1 {
		t.Fatalf("absent wsrep_last_committed must be -1, got %d", empty.LastCommitted)
	}
}
