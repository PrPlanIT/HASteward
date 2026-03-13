package model

import "time"

// Canonical event types.
const (
	EventRunStarted    = "run.started"
	EventRunCompleted  = "run.completed"
	EventPhaseStarted  = "phase.started"
	EventPhaseComplete = "phase.completed"
	EventCheckPassed   = "check.passed"
	EventCheckFailed   = "check.failed"
	EventStepStarted   = "step.started"
	EventStepComplete  = "step.completed"
)

// Canonical phase names for dangerous commands.
const (
	PhasePreflight      = "preflight"
	PhaseSuspend        = "suspend"
	PhaseScaleDown      = "scale_down"
	PhaseBootstrapMark  = "bootstrap_mark"
	PhaseClusterPatch   = "cluster_patch"
	PhaseScaleUp        = "scale_up"
	PhaseWaitReady      = "wait_ready"
	PhaseCleanup        = "cleanup"
	PhaseVerify         = "verify"
	PhaseGenLock        = "gen_lock"
	PhaseWsrepRecover   = "wsrep_recover"
	PhaseSafeBootClear  = "safe_boot_clear"
	PhaseResume         = "resume"
)

// Event represents a discrete progress event emitted during command execution.
type Event struct {
	Type      string         `json:"type"`
	Command   string         `json:"command,omitempty"`
	RunID     string         `json:"runId,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
	Phase     string         `json:"phase,omitempty"`
	Message   string         `json:"message,omitempty"`
	Resource  *ObjectRef     `json:"resource,omitempty"`
	Success   *bool          `json:"success,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// NewEvent creates an Event with the current timestamp.
func NewEvent(eventType, command, runID string) Event {
	return Event{
		Type:      eventType,
		Command:   command,
		RunID:     runID,
		Timestamp: time.Now().UTC(),
	}
}

// WithPhase sets the phase on an event.
func (e Event) WithPhase(phase string) Event {
	e.Phase = phase
	return e
}

// WithMessage sets the message on an event.
func (e Event) WithMessage(msg string) Event {
	e.Message = msg
	return e
}

// WithResource sets the resource reference on an event.
func (e Event) WithResource(ref ObjectRef) Event {
	e.Resource = &ref
	return e
}

// WithSuccess sets the success flag on an event.
func (e Event) WithSuccess(s bool) Event {
	e.Success = &s
	return e
}

// WithDetails sets additional details on an event.
func (e Event) WithDetails(details map[string]any) Event {
	e.Details = details
	return e
}

// CompletionEvent creates a run.completed event with final status.
func CompletionEvent(command, runID string, success, partial bool, exitCode int, warnings []Warning, errors []StructuredError) Event {
	details := map[string]any{
		"exitCode": exitCode,
		"partial":  partial,
	}
	if len(warnings) > 0 {
		details["warningCount"] = len(warnings)
	}
	if len(errors) > 0 {
		details["errorCount"] = len(errors)
	}

	evt := NewEvent(EventRunCompleted, command, runID)
	evt.Success = &success
	evt.Details = details
	return evt
}
