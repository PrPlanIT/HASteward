package common

import "time"

// InstanceAssessment holds the triage assessment for a single database instance.
type InstanceAssessment struct {
	Pod          string   `json:"pod"`
	Instance     int      `json:"instance"`
	IsRunning    bool     `json:"is_running"`
	IsReady      bool     `json:"is_ready"`
	NeedsHeal    bool     `json:"needs_heal"`
	Notes        []string `json:"notes"`
	Recommendation string `json:"recommendation"`

	// CNPG-specific
	IsPrimary  bool   `json:"is_primary,omitempty"`
	Timeline   int64  `json:"timeline,omitempty"`
	LSN        string `json:"lsn,omitempty"`

	// Galera-specific
	IsInPrimary        bool   `json:"is_in_primary,omitempty"`
	Seqno              int64  `json:"seqno,omitempty"`
	EffectiveSeqno     int64  `json:"effective_seqno,omitempty"`
	SeqnoSource        string `json:"seqno_source,omitempty"`
	SeqnoLag           int64  `json:"seqno_lag"`
	UUID               string `json:"uuid,omitempty"`
	SafeToBootstrap    string `json:"safe_to_bootstrap,omitempty"`
	WsrepState         int    `json:"wsrep_state,omitempty"`
	WsrepStateComment  string `json:"wsrep_state_comment,omitempty"`
	WsrepConnected     string `json:"wsrep_connected,omitempty"`
	WsrepReady         string `json:"wsrep_ready,omitempty"`
	WsrepClusterStatus string `json:"wsrep_cluster_status,omitempty"`
	CrashReason        string `json:"crash_reason,omitempty"`
	DiskPct            int    `json:"disk_pct"`
}

// DataComparison holds the cross-instance data comparison results.
type DataComparison struct {
	MostAdvanced      string   `json:"most_advanced"`
	MostAdvancedValue int64    `json:"most_advanced_value"`
	SafeToHeal        bool     `json:"safe_to_heal"`
	Warnings          []string `json:"warnings,omitempty"`
	SplitBrainDetails []string `json:"split_brain_details,omitempty"`

	// CNPG-specific
	CheckpointLocation string `json:"checkpoint_location,omitempty"`

	// Galera-specific
	PrimaryMembers   []string `json:"primary_members,omitempty"`
	BestPrimarySeqno int64    `json:"best_primary_seqno,omitempty"`
}

// TriageResult holds the complete triage output for a cluster.
type TriageResult struct {
	Assessments    []InstanceAssessment `json:"assessments"`
	DataComparison DataComparison       `json:"data_comparison"`
	ClusterPhase   string               `json:"cluster_phase"`
	ReadyCount     int                  `json:"ready_count"`
	TotalCount     int                  `json:"total_count"`

	// Galera-specific
	AllNodesDown    bool              `json:"all_nodes_down,omitempty"`
	BestSeqnoNode   *InstanceAssessment `json:"best_seqno_node,omitempty"`
}

// BackupResult holds the outcome of a backup operation.
type BackupResult struct {
	SnapshotID string            `json:"snapshot_id"`
	Repository string            `json:"repository"`
	Size       int64             `json:"size_bytes"`
	Duration   time.Duration     `json:"duration"`
	Tags       map[string]string `json:"tags"`
}

// RepairResult holds the outcome of a repair operation.
type RepairResult struct {
	HealedInstances  []string      `json:"healed_instances"`
	SkippedInstances []string      `json:"skipped_instances"`
	Duration         time.Duration `json:"duration"`
	PostTriageResult *TriageResult `json:"post_triage,omitempty"`
}

// RestoreResult holds the outcome of a restore operation.
type RestoreResult struct {
	SnapshotID string        `json:"snapshot_id"`
	Duration   time.Duration `json:"duration"`
}

// EventSink receives progress events from engine operations.
// Implementations format these for CLI output, slog, metrics, etc.
type EventSink interface {
	Info(msg string, fields ...any)
	Warn(msg string, fields ...any)
	Progress(operation string, current, total int64)
	Step(name string, status string)
}

// NopEventSink discards all events.
type NopEventSink struct{}

func (NopEventSink) Info(string, ...any)          {}
func (NopEventSink) Warn(string, ...any)          {}
func (NopEventSink) Progress(string, int64, int64) {}
func (NopEventSink) Step(string, string)           {}

// Config holds all runtime configuration for a hasteward run.
type Config struct {
	Engine         string
	ClusterName    string
	Namespace      string
	Mode           string
	InstanceNumber *int
	Force          bool
	BackupsPath    string
	NoEscrow       bool
	BackupMethod   string
	Snapshot       string // Restic snapshot ID or "latest" (for restore)
	ResticPassword string // Restic repository encryption password
	HealTimeout    int
	DeleteTimeout  int
	Kubeconfig     string
	Verbose        bool
}
