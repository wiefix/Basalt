package cloud_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/wiefix/basalt/internal/cloud"
	"github.com/wiefix/basalt/internal/cloud/cloudtest"
)

func TestHetzner(t *testing.T) {
	fake := cloudtest.NewHetzner(t, "tok")
	fake.AddServer(cloudtest.Server{ID: 1, Name: "a", IPv4: "198.51.100.1", IPv6: "2001:db8:a::/64"})
	fake.AddServer(cloudtest.Server{ID: 2, Name: "b", IPv6: "2001:db8:b::/64"})
	fake.AddFloatingIP(cloudtest.FloatingIP{ID: 10, Name: "web", IP: "203.0.113.10", Location: "fsn1", Server: 1})
	fake.AddFloatingIP(cloudtest.FloatingIP{ID: 11, Name: "web6", IP: "2001:db8:f::/64", Location: "hel1"})
	ctx := context.Background()

	bad, _ := cloud.New(cloud.Hetzner, "wrong", fake.URL)
	if _, err := bad.FloatingIPs(ctx); !cloud.Unauthorized(err) {
		t.Fatalf("wrong token: %v", err)
	}

	p, err := cloud.New(cloud.Hetzner, "tok", fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	ips, err := p.FloatingIPs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 || ips[0] != (cloud.FloatingIP{ID: "10", Name: "web", Address: "203.0.113.10", Network: "203.0.113.10", Location: "fsn1", ServerID: "1"}) ||
		ips[1].Address != "2001:db8:f::1" || ips[1].Network != "2001:db8:f::/64" || ips[1].ServerID != "" {
		t.Fatalf("floating IPs: %+v", ips)
	}
	if _, err := p.FloatingIP(ctx, "99"); !errors.Is(err, cloud.ErrNotFound) {
		t.Fatalf("missing floating IP: %v", err)
	}
	if _, err := p.FloatingIP(ctx, "../servers"); !errors.Is(err, cloud.ErrNotFound) {
		t.Fatalf("invalid id: %v", err)
	}

	servers, err := p.Servers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[1].IPv4 != "" || servers[1].IPv6 != "2001:db8:b::/64" {
		t.Fatalf("servers: %+v", servers)
	}
	if !servers[0].Matches(netip.MustParseAddr("198.51.100.1")) || !servers[1].Matches(netip.MustParseAddr("2001:db8:b::5")) ||
		servers[0].Matches(netip.MustParseAddr("2001:db8:b::5")) {
		t.Fatal("matching addresses")
	}

	if err := p.Assign(ctx, "11", "2"); err != nil {
		t.Fatal(err)
	}
	if fake.AssignedTo(11) != 2 {
		t.Fatal("not assigned")
	}
	fake.FailAssign(true)
	if err := p.Assign(ctx, "11", "1"); err == nil || fake.AssignedTo(11) != 2 {
		t.Fatalf("failed assignment: %v", err)
	}
	fake.FailAssign(false)
	if err := p.Assign(ctx, "11", "7"); err == nil {
		t.Fatal("unknown server assigned")
	}
}

func TestValidToken(t *testing.T) {
	for _, tok := range []string{"", "a b", "a\nb", string(make([]byte, 600))} {
		if cloud.ValidToken(tok) == nil {
			t.Errorf("%q accepted", tok)
		}
	}
	if err := cloud.ValidToken("abcDEF123"); err != nil {
		t.Error(err)
	}
}
