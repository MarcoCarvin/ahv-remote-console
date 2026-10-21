// Package build holds compile-time-injected metadata for the agent.
// The values are set by the Makefile via -ldflags -X so this package
// has no runtime dependencies.
package build

// Set by `-ldflags -X github.com/.../internal/build.Version=…` at link
// time. Defaults are placeholders for `go run` / `go test` invocations
// that bypass the Makefile.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info returns a one-line human-readable build identifier suitable
// for logging.
func Info() string {
	return "ahv-remote-console-agent " + Version + " (" + Commit + ", built " + Date + ")"
}
