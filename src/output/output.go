package output

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
)

const bannerWidth = 60

// writer is the package-level destination for all output functions.
// Defaults to os.Stdout; set to io.Discard in machine-output modes.
var writer io.Writer = os.Stdout

// enabled controls whether output functions produce any output.
// When false (json/jsonl modes), all functions become no-ops.
var enabled = true

// SetWriter sets the package-level writer for all output functions.
func SetWriter(w io.Writer) {
	writer = w
}

// SetEnabled controls whether output functions produce any output.
// Call SetEnabled(false) in json/jsonl modes to silence legacy output.
func SetEnabled(v bool) {
	enabled = v
	if !v {
		writer = io.Discard
	} else {
		writer = os.Stdout
	}
}

// Enabled returns whether legacy output is currently active.
func Enabled() bool {
	return enabled
}

// Writer returns the current writer (os.Stdout or io.Discard).
func Writer() io.Writer {
	return writer
}

// Printf is a mode-aware wrapper around fmt.Fprintf to the package writer.
// In json/jsonl modes this is a no-op.
func Printf(format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, format, args...)
}

// Println is a mode-aware wrapper around fmt.Fprintln to the package writer.
// In json/jsonl modes this is a no-op.
func Println(args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintln(writer, args...)
}

// Banner prints a prominent section header.
func Banner(title string) {
	if !enabled {
		return
	}
	line := strings.Repeat("=", bannerWidth)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, line)
	fmt.Fprintf(writer, "  %s\n", title)
	fmt.Fprintln(writer, line)
	fmt.Fprintln(writer)
}

// Header prints a formatted header line for HASteward startup.
func Header(engine, mode, clusterName, namespace string) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "=== HASteward / %s (%s) ===\n", strings.ToUpper(engine), mode)
	if engine == "galera" {
		fmt.Fprintf(writer, "MariaDB: %s\n", clusterName)
	} else {
		fmt.Fprintf(writer, "Cluster: %s\n", clusterName)
	}
	fmt.Fprintf(writer, "Namespace: %s\n", namespace)
	fmt.Fprintf(writer, "Timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))
}

// Section prints a subsection divider.
func Section(title string) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "--- %s ---\n", title)
}

// Field prints a labeled value.
func Field(label, value string) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "%s: %s\n", label, value)
}

// Bullet prints a bulleted item with optional indentation.
func Bullet(indent int, format string, args ...any) {
	if !enabled {
		return
	}
	prefix := strings.Repeat("  ", indent)
	fmt.Fprintf(writer, "%s- %s\n", prefix, fmt.Sprintf(format, args...))
}

// Info prints an informational line prefixed with >> (recommendation style).
func Info(format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "  >> %s\n", fmt.Sprintf(format, args...))
}

// Plan announces what a --dry-run WOULD do. Unlike Info it also goes to the
// structured log, because the human writer is io.Discard whenever stdout is not a
// TTY: OutputAuto resolves to JSON there, and SetEnabled(false) silences every
// output.* call. A dry run in a Kubernetes Job or CI therefore printed its safety
// gates and nothing else — no plan at all — which is the one context where the
// preview is the whole point. Logging it means the operator sees it in
// `kubectl logs` regardless of the rendering mode.
func Plan(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	common.InfoLog("%s", msg)
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "  >> %s\n", msg)
}

// Success prints a success message.
func Success(format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "[OK] %s\n", fmt.Sprintf(format, args...))
}

// Warn prints a warning message to stdout.
func Warn(format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "[WARN] %s\n", fmt.Sprintf(format, args...))
}

// Fail prints a failure message to stdout.
func Fail(format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "[FAIL] %s\n", fmt.Sprintf(format, args...))
}

// Stderr writes to stderr unconditionally, regardless of output mode.
// Used for CLI messages that must not mix with stdout data (errors, status).
func Stderr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// Complete prints a completion message.
func Complete(msg string) {
	if !enabled {
		return
	}
	fmt.Fprintf(writer, "=== %s ===\n", msg)
}

// SuggestedCommands prints suggested repair commands for triage output.
func SuggestedCommands(engine, clusterName, namespace string) {
	if !enabled {
		return
	}
	fmt.Fprintln(writer)
	Section("Suggested Commands")
	fmt.Fprintln(writer, "Repair all unhealthy instances:")
	fmt.Fprintf(writer, "  hasteward repair -e %s -c %s -n %s --backups-path /backups\n", engine, clusterName, namespace)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "Repair a specific instance:")
	fmt.Fprintf(writer, "  hasteward repair -e %s -c %s -n %s --instance <N> --backups-path /backups\n", engine, clusterName, namespace)
}

// FormatBytes returns a human-readable byte size.
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
