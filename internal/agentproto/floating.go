package agentproto

import (
	"fmt"
	"net/netip"
)

// TypeFloatingIPs gives an agent every floating IP its host configures
// (FloatingIPs).
const TypeFloatingIPs = "floating_ips"

// MaxFloatingIPs bounds the floating IPs of a server.
const MaxFloatingIPs = 64

// FloatingIPs is every floating IP a host configures (of all the groups
// it is in): the provider routes each to one server, and every server of
// the group has it on its interface, so moving one is only a call to the
// provider.
type FloatingIPs struct {
	Addresses []string `json:"addresses"`
}

// Validate checks that every address is a public unicast one, written
// the canonical way.
func (f FloatingIPs) Validate() error {
	if len(f.Addresses) > MaxFloatingIPs {
		return fmt.Errorf("at most %d floating IPs", MaxFloatingIPs)
	}
	seen := map[netip.Addr]bool{}
	for _, s := range f.Addresses {
		a, err := netip.ParseAddr(s)
		if err != nil || a.String() != s || a.Is4In6() || !a.IsGlobalUnicast() || a.IsPrivate() {
			return fmt.Errorf("invalid floating IP %q", s)
		}
		if seen[a] {
			return fmt.Errorf("floating IP %s given twice", s)
		}
		seen[a] = true
	}
	return nil
}
