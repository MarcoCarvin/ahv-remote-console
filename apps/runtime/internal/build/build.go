// Package build holds compile-time build metadata. Values are injected via
// -ldflags at build time and remain "dev" / "unknown" for go run.
package build

import "fmt"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func Info() string {
	return fmt.Sprintf("ahv-remote-console-runtime %s (commit %s, built %s)", Version, Commit, Date)
}
