package privhelper

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

// TestFloatingIPsLinux puts floating IPs on the interface with the real
// iproute2 and takes them away (make test-linux: a container of its own
// with CAP_NET_ADMIN; BASALT_NET_TESTS guards the host's network).
func TestFloatingIPsLinux(t *testing.T) {
	if os.Getenv("BASALT_NET_TESTS") == "" || os.Geteuid() != 0 {
		t.Skip("set BASALT_NET_TESTS=1 and run as root in a disposable container")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 is not installed")
	}
	h, err := New(Config{SitesRoot: "/srv/basalt/sites", StateDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	iface, err := defaultInterface(ctx, runIP)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.77", "2001:db8:77::1"}
	// Docker turns IPv6 off on a container's interface of an IPv4 network.
	if b, _ := os.ReadFile("/proc/sys/net/ipv6/conf/" + iface + "/disable_ipv6"); strings.TrimSpace(string(b)) != "0" {
		t.Logf("IPv6 is off on %s: IPv4 only", iface)
		want = want[:1]
	}
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: want}); err != nil {
		t.Fatal(err)
	}
	have, err := addressesOn(ctx, runIP, iface)
	if err != nil {
		t.Fatal(err)
	}
	if have["203.0.113.77"] != 32 || len(want) > 1 && have["2001:db8:77::1"] != 128 {
		t.Fatalf("%s has %v", iface, have)
	}
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: want}); err != nil {
		t.Fatalf("again: %v", err)
	}
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: []string{}}); err != nil {
		t.Fatal(err)
	}
	have, _ = addressesOn(ctx, runIP, iface)
	if have["203.0.113.77"] != 0 || have["2001:db8:77::1"] != 0 {
		t.Fatalf("%s still has %v", iface, have)
	}
}
