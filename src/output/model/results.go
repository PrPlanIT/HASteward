package model

import "time"

// InstanceAssessment holds the triage assessment for a single database instance.
type InstanceAssessment struct {
	Pod            string   `json:"pod"`
	Instance       int      `json:"instance"`
	IsRunning      bool     `json:"isRunning"`
	IsReady        bool     `json:"isReady"`
	NeedsHeal      bool     `json:"needsHeal"`
	Notes          []string `json:"notes"`
	Recommendation string   `json:"recommendation"`

	// CNPG-specific
	IsPrimary bool   `json:"isPrimary,omitempty"`
	Timeline  int64  `json:"timeline,omitempty"`
	LSN       string `json:"lsn,omitempty"`

	// Galera-specific
	IsInPrimary        bool   `json:"isInPrimary,omitempty"`
	Seqno              int64  `json:"seqno,omitempty"`
	EffectiveSeqno     int64  `json:"effectiveSeqno,omitempty"`
	SeqnoSource        string `json:"seqnoSource,omitempty"`
	SeqnoLag           int64  `json:"seqnoLag"`
	UUID               string `json:"uuid,omitempty"`
	SafeToBootstrap    string `json:"safeToBootstrap,omitempty"`
	WsrepState         int    `json:"wsrepState,omitempty"`
	WsrepStateComment  string `json:"wsrepStateComment,omitempty"`
	WsrepConnected     string `json:"wsrepConnected,omitempty"`
	WsrepReady         string `json:"wsrepReady,omitempty"`
	WsrepClusterStatus string `json:"wsrepClusterStatus,omitempty"`
	CrashReason        string `json:"crashReason,omitempty"`
	DiskPct            int    `json:"diskPct"`
}

// DataComparison holds the cross-instance data comparison results.
type DataComparison struct {
	MostAdvanced      string   `json:"mostAdvanced"`
	MostAdvancedValue int64    `json:"mostAdvancedValue"`
	SafeToHeal        bool     `json:"safeToHeal"`
	Warnings          []string `json:"warnings,omitempty"`
	SplitBrainDetails []string `json:"splitBrainDetails,omitempty"`

	// CNPG-specific
	CheckpointLocation string `json:"checkpointLocation,omitempty"`

	// Galera-specific
	PrimaryMembers   []string `json:"primaryMembers,omitempty"`
	BestPrimarySeqno int64    `json:"bestPrimarySeqno,omitempty"`
}

// ClusterHealthSummary is an abbreviated health status for result embedding.
type ClusterHealthSummary struct {
	ReadyCount int    `json:"readyCount"`
	TotalCount int    `json:"totalCount"`
	Phase      string `json:"phase,omitempty"`
	Healthy    bool   `json:"healthy"`
}

// TriageResult holds the complete triage output for a cluster.
type TriageResult struct {
	Engine         string               `json:"engine"`
	Cluster        ObjectRef            `json:"cluster"`
	Assessments    []InstanceAssessment `json:"assessments"`
	DataComparison DataComparison       `json:"dataComparison"`
	ClusterPhase   string               `json:"clusterPhase"`
	ReadyCount     int                  `json:"readyCount"`
	TotalCount     int                  `json:"totalCount"`

	// Galera-specific
	AllNodesDown  bool                `json:"allNodesDown,omitempty"`
	BestSeqnoNode *InstanceAssessment `json:"bestSeqnoNode,omitempty"`
}

// BackupResult holds the outcome of a backup operation.
type BackupResult struct {
	Engine     string            `json:"engine"`
	Cluster    ObjectRef         `json:"cluster"`
	SnapshotID string            `json:"snapshotId"`
	Repository string            `json:"repository"`
	Size       int64             `json:"sizeBytes"`
	Duration   time.Duration     `json:"duration"`
	Tags       map[string]string `json:"tags"`
}

// RepairResult holds the outcome of a repair operation.
type RepairResult struct {
	Engine           string        `json:"engine"`
	Cluster          ObjectRef     `json:"cluster"`
	HealedInstances  []string      `json:"healedInstances"`
	SkippedInstances []string      `json:"skippedInstances"`
	Duration         time.Duration `json:"duration"`
	PostTriageResult *TriageResult `json:"postTriage,omitempty"`
}

// RestoreResult holds the outcome of a restore operation.
type RestoreResult struct {
	Engine     string        `json:"engine"`
	Cluster    ObjectRef     `json:"cluster"`
	SnapshotID string        `json:"snapshotId"`
	Duration   time.Duration `json:"duration"`
}

// BootstrapDecision captures the eligibility analysis for a Galera bootstrap.
type BootstrapDecision struct {
	Eligible            bool            `json:"eligible"`
	Reason              string          `json:"reason"`
	CandidatePod        string          `json:"candidatePod"`
	CandidateSeqno      int64           `json:"candidateSeqno"`
	CandidateUUID       string          `json:"candidateUuid"`
	AmbiguityDetected   bool            `json:"ambiguityDetected"`
	ForceRequired       bool            `json:"forceRequired"`
	SafeToProceed       bool            `json:"safeToProceed"`
	Competitors         []string        `json:"competitors,omitempty"`
	WsrepRecoverApplied bool            `json:"wsrepRecoverApplied,omitempty"`
	OriginalCandidate   string          `json:"originalCandidate,omitempty"`
	LineageGroups       []LineageGroup  `json:"lineageGroups,omitempty"`
}

// LineageGroup represents a set of nodes sharing a recovery UUID lineage.
type LineageGroup struct {
	UUID         string   `json:"uuid"`
	Members      []string `json:"members"`
	MaxSeqno     int64    `json:"maxSeqno"`
	MaxCommitted int64    `json:"maxCommitted"`
	BestNode     string   `json:"bestNode"`
}

// BootstrapAction describes a single mutation in a bootstrap sequence.
type BootstrapAction struct {
	Phase       string     `json:"phase"`
	Description string     `json:"description"`
	Resource    *ObjectRef `json:"resource,omitempty"`
	Completed   bool       `json:"completed"`
}

// BootstrapResult holds the full outcome of a Galera bootstrap operation.
type BootstrapResult struct {
	Engine         string                `json:"engine"`
	Cluster        ObjectRef             `json:"cluster"`
	Decision       BootstrapDecision     `json:"decision"`
	ActionsPlanned []BootstrapAction     `json:"actionsPlanned,omitempty"`
	ActionsTaken   []BootstrapAction     `json:"actionsTaken,omitempty"`
	FinalHealth    *ClusterHealthSummary `json:"finalHealth,omitempty"`
}

// GetBackupsResult holds the output of "get backups".
type GetBackupsResult struct {
	Snapshots []SnapshotEntry `json:"snapshots"`
}

// SnapshotEntry represents a single backup snapshot in "get backups" output.
type SnapshotEntry struct {
	Repository string `json:"repository"`
	SnapshotID string `json:"snapshotId"`
	Type       string `json:"type"`
	Engine     string `json:"engine"`
	Namespace  string `json:"namespace"`
	Cluster    string `json:"cluster"`
	Age        string `json:"age"`
}

// GetPoliciesResult holds the output of "get policies".
type GetPoliciesResult struct {
	Policies []PolicyEntry `json:"policies"`
}

// PolicyEntry represents a single BackupPolicy in "get policies" output.
type PolicyEntry struct {
	Name           string   `json:"name"`
	BackupSchedule string   `json:"backupSchedule"`
	TriageSchedule string   `json:"triageSchedule"`
	Mode           string   `json:"mode"`
	Repositories   []string `json:"repositories,omitempty"`
}

// GetRepositoriesResult holds the output of "get repositories".
type GetRepositoriesResult struct {
	Repositories []RepositoryEntry `json:"repositories"`
}

// RepositoryEntry represents a single BackupRepository in "get repositories" output.
type RepositoryEntry struct {
	Name             string `json:"name"`
	Repository       string `json:"repository"`
	Ready            bool   `json:"ready"`
	SnapshotCount    int64  `json:"snapshotCount"`
	TotalSize        string `json:"totalSize"`
	DeduplicatedSize string `json:"deduplicatedSize"`
}

// GetStatusResult holds the output of "get status".
type GetStatusResult struct {
	Clusters []ClusterStatusEntry `json:"clusters"`
}

// ClusterStatusEntry represents a managed database cluster in "get status" output.
type ClusterStatusEntry struct {
	Engine       string `json:"engine"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Managed      string `json:"managed"`
	TriageResult string `json:"triageResult"`
	LastTriage   string `json:"lastTriage"`
	LastBackup   string `json:"lastBackup"`
}

// PruneResult holds the output of "prune backups".
type PruneResult struct {
	TotalKept    int `json:"totalKept"`
	TotalRemoved int `json:"totalRemoved"`
}

// PruneWALResult holds the output of "prune wal".
type PruneWALResult struct {
	Engine   string    `json:"engine"`
	Cluster  ObjectRef `json:"cluster"`
	Instance int64     `json:"instance"`
}

// ExportResult holds the output of "export".
type ExportResult struct {
	OutputFile string `json:"outputFile"`
	Snapshot   string `json:"snapshot"`
	Engine     string `json:"engine"`
	Cluster    string `json:"cluster"`
	Namespace  string `json:"namespace"`
}
