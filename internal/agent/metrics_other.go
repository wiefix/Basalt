//go:build !linux

package agent

import (
	"runtime"

	"github.com/wiefix/basalt/internal/agentproto"
)

// Managed hosts run Linux. On other systems (developer machines) the agent
// still runs, reporting disk usage only, so the full flow can be exercised.
func (c *collector) collectPlatform(m *agentproto.Metrics) error { return nil }

func platformFacts() (kernel, distro string, memTotal uint64) {
	return "", runtime.GOOS + " (unsupported for hosting)", 0
}
