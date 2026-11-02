// Package version carries build information injected with -ldflags.
package version

import "fmt"

var (
	// Version is the application version, e.g. "v0.1.0".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the build timestamp.
	BuildDate = "unknown"
)

// String returns a human readable build identifier.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, BuildDate)
}
