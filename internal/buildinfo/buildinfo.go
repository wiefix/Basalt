// Package buildinfo exposes version metadata injected at build time via -ldflags.
package buildinfo

import "fmt"

// Set with: -ldflags "-X github.com/wiefix/basalt/internal/buildinfo.Version=..."
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return fmt.Sprintf("basalt %s (commit %s, built %s)", Version, Commit, Date)
}
