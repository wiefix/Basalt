//go:build linux_integration

package e2e

import (
	"os"
	"testing"

	"github.com/wiefix/basalt/internal/siteworker"
)

// TestMain lets the test binary be the agent's site worker too, as the
// basalt binary is ("<exe> site-worker …").
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == siteworker.Command {
		os.Exit(siteworker.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
