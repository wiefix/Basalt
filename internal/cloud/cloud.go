// Package cloud talks to the APIs of cloud providers (the panel side):
// their floating IPs, which the panel moves between servers, and their
// servers, which it matches to its own by address.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Providers the panel knows.
const (
	Hetzner = "hetzner"
)

// ValidProvider reports whether p is a provider the panel knows.
func ValidProvider(p string) bool { return p == Hetzner }

// FloatingIP is a provider's address that can be routed to any of its
// servers.
type FloatingIP struct {
	ID   string
	Name string
	// Address is what a server configures on its interface: the IPv4
	// address, or the first address of an IPv6 block. Network is the
	// address or block as the provider names it ("2001:db8::/64").
	Address  string
	Network  string
	Location string
	// ServerID is the provider's server the address is routed to ("": none).
	ServerID string
}

// Server is one of the provider's servers.
type Server struct {
	ID   string
	Name string
	// IPv4 is its public IPv4 address ("": none); IPv6 its IPv6 block.
	IPv4 string
	IPv6 string
}

// Matches reports whether addr is one of the server's own public
// addresses.
func (s Server) Matches(addr netip.Addr) bool {
	addr = addr.Unmap()
	if v4, err := netip.ParseAddr(s.IPv4); err == nil && v4 == addr {
		return true
	}
	if p, err := netip.ParsePrefix(s.IPv6); err == nil && p.Contains(addr) {
		return true
	}
	return false
}

// Provider is a cloud provider's API with one account's token.
type Provider interface {
	// FloatingIPs lists the account's floating IPs; FloatingIP reads one
	// (ErrNotFound if it is gone).
	FloatingIPs(ctx context.Context) ([]FloatingIP, error)
	FloatingIP(ctx context.Context, id string) (*FloatingIP, error)
	// Servers lists the account's servers.
	Servers(ctx context.Context) ([]Server, error)
	// Assign routes a floating IP to a server and returns once the
	// provider has done it.
	Assign(ctx context.Context, ipID, serverID string) error
}

var ErrNotFound = errors.New("not found at the provider")

// Error is an error the provider's API answered with.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	return fmt.Sprintf("provider API: %s (HTTP %d)", strings.TrimSuffix(msg, "."), e.Status)
}

// Unauthorized reports whether the token was refused.
func Unauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == 401 || e.Status == 403)
}

// ValidToken checks the shape of an API token (its validity only the
// provider knows).
func ValidToken(token string) error {
	if token == "" || len(token) > 512 {
		return errors.New("API token is required (at most 512 characters)")
	}
	for _, c := range token {
		if c <= ' ' || c > '~' {
			return errors.New("API token has invalid characters")
		}
	}
	return nil
}

// New returns the API of provider with token; api replaces the provider's
// address (tests), "" for the real one.
func New(provider, token, api string) (Provider, error) {
	switch provider {
	case Hetzner:
		return newHetzner(token, api), nil
	}
	return nil, fmt.Errorf("unknown cloud provider %q", provider)
}
