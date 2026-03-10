package common

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var (
	secretsMu sync.RWMutex
	secrets   []string
)

// InitLogging sets up the slog-based logging system.
// jsonMode=true uses JSONHandler (for serve mode), false uses TextHandler (for CLI).
func InitLogging(jsonMode bool) {
	level := resolveLogLevel()
	var handler slog.Handler
	if jsonMode {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	handler = &sanitizingHandler{inner: handler}
	slog.SetDefault(slog.New(handler))
}

func resolveLogLevel() slog.Level {
	switch strings.ToLower(Env("LOG_LEVEL", "info")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// RegisterSecret adds a value to be redacted from all log output.
func RegisterSecret(value string) {
	if value == "" {
		return
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	secrets = append(secrets, value)
}

// sanitize replaces registered secrets with [REDACTED] in the output.
func sanitize(msg string) string {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for _, s := range secrets {
		msg = strings.ReplaceAll(msg, s, "[REDACTED]")
	}
	return msg
}

// sanitizingHandler wraps an slog.Handler and redacts registered secrets
// from message text and string attribute values.
type sanitizingHandler struct {
	inner slog.Handler
}

func (h *sanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *sanitizingHandler) Handle(ctx context.Context, r slog.Record) error {
	sanitized := slog.NewRecord(r.Time, r.Level, sanitize(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		sanitized.AddAttrs(sanitizeAttr(a))
		return true
	})
	return h.inner.Handle(ctx, sanitized)
}

func (h *sanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sanitizingHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *sanitizingHandler) WithGroup(name string) slog.Handler {
	return &sanitizingHandler{inner: h.inner.WithGroup(name)}
}

func sanitizeAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(sanitize(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		sanitized := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			sanitized[i] = sanitizeAttr(ga)
		}
		a.Value = slog.GroupValue(sanitized...)
	}
	return a
}

// Backward-compatible logging functions.
// These wrap slog with printf-style formatting to avoid rewriting every caller.

func DebugLog(format string, args ...any) {
	slog.Debug(fmt.Sprintf(format, args...))
}

func InfoLog(format string, args ...any) {
	slog.Info(fmt.Sprintf(format, args...))
}

func WarnLog(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

func ErrorLog(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
}

func FatalLog(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
