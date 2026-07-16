package version

import "fmt"

// These variables are set at build time via ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String returns a human version summary: "<version> (<commit>, built <date>)".
func String() string {
	return fmt.Sprintf("%s (%s, built %s)", Version, Commit, BuildDate)
}
