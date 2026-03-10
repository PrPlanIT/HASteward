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
