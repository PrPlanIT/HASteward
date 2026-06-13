package common

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
	DonorInstance  *int // Explicit donor ordinal for forced repair (declares authoritative source)
	Force          bool
	WipeDatadir    bool // Wipe entire datadir (not just grastate) — forces full SST reseed from donor
	FixBootstrap   bool // Reconfigure: clear grastate + remove bootstrap config on target instance
	BackupsPath    string
	NoEscrow       bool
	Unwedge        bool // --unwedge: enable the CNPG disk-full deadlock breaker (escrow-gated offline datadir clear)
	BackupMethod   string
	Snapshot       string // Restic snapshot ID or "latest" (for restore)
	ResticPassword string // Restic repository encryption password
	HealTimeout    int
	DeleteTimeout  int
	Kubeconfig     string
	Verbose        bool
	DryRun         bool // preview destructive actions without executing (set from --dry-run)
}
