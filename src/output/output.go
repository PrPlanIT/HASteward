package output

import (
	"fmt"
	"strings"
	"time"
)

const bannerWidth = 60

// Banner prints a prominent section header.
func Banner(title string) {
	line := strings.Repeat("=", bannerWidth)
	fmt.Println()
	fmt.Println(line)
	fmt.Printf("  %s\n", title)
	fmt.Println(line)
	fmt.Println()
}

// Header prints a formatted header line for HASteward startup.
func Header(engine, mode, clusterName, namespace string) {
	fmt.Printf("=== HASteward / %s (%s) ===\n", strings.ToUpper(engine), mode)
	if engine == "galera" {
		fmt.Printf("MariaDB: %s\n", clusterName)
	} else {
		fmt.Printf("Cluster: %s\n", clusterName)
	}
	fmt.Printf("Namespace: %s\n", namespace)
	fmt.Printf("Timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))
}

// Section prints a subsection divider.
func Section(title string) {
	fmt.Printf("--- %s ---\n", title)
}

// Field prints a labeled value.
func Field(label, value string) {
	fmt.Printf("%s: %s\n", label, value)
}

// Bullet prints a bulleted item with optional indentation.
func Bullet(indent int, format string, args ...any) {
	prefix := strings.Repeat("  ", indent)
	fmt.Printf("%s- %s\n", prefix, fmt.Sprintf(format, args...))
}

// Info prints an informational line prefixed with >> (recommendation style).
func Info(format string, args ...any) {
	fmt.Printf("  >> %s\n", fmt.Sprintf(format, args...))
}

// Success prints a success message.
func Success(format string, args ...any) {
	fmt.Printf("[OK] %s\n", fmt.Sprintf(format, args...))
}

// Warn prints a warning message to stdout.
func Warn(format string, args ...any) {
	fmt.Printf("[WARN] %s\n", fmt.Sprintf(format, args...))
}

// Fail prints a failure message to stdout.
func Fail(format string, args ...any) {
	fmt.Printf("[FAIL] %s\n", fmt.Sprintf(format, args...))
}

// Complete prints a completion message.
func Complete(msg string) {
	fmt.Printf("=== %s ===\n", msg)
}

// SuggestedCommands prints suggested repair commands for triage output.
func SuggestedCommands(engine, clusterName, namespace string) {
	fmt.Println()
	Section("Suggested Commands")
	fmt.Println("Repair all unhealthy instances:")
	fmt.Printf("  hasteward repair -e %s -c %s -n %s --backups-path /backups\n", engine, clusterName, namespace)
	fmt.Println()
	fmt.Println("Repair a specific instance:")
	fmt.Printf("  hasteward repair -e %s -c %s -n %s --instance <N> --backups-path /backups\n", engine, clusterName, namespace)
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
