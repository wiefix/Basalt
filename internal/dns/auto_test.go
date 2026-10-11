package dns

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAutoRecords(t *testing.T) {
	ip := netip.MustParseAddr
	z := Zone{Name: "example.com", Records: []Record{
		rec("@", TypeNS, "ns1.example.net"),
		rec("manual", TypeA, "192.0.2.99"),
		rec("alias", TypeCNAME, "elsewhere.example.org"),
		rec("txt", TypeTXT, "hello"),
		rec("sub", TypeNS, "ns.other.example"),
	}}
	targets := []AutoTarget{
		{Domain: "example.com", IPs: []netip.Addr{ip("203.0.113.1"), ip("2001:db8::1")}, Source: "site blog"},
		{Domain: "www.example.com", IPs: []netip.Addr{ip("203.0.113.1")}, Source: "site blog"},
		// The same domain on two servers: both addresses, both sources.
		{Domain: "app.example.com", IPs: []netip.Addr{ip("203.0.113.1")}, Source: "app a"},
		{Domain: "app.example.com", IPs: []netip.Addr{ip("203.0.113.2"), ip("203.0.113.1")}, Source: "app b"},
		{Domain: "txt.example.com", IPs: []netip.Addr{ip("203.0.113.3")}, Source: "site t"},
		// Manual records, delegations and other zones are left alone.
		{Domain: "manual.example.com", IPs: []netip.Addr{ip("203.0.113.4")}, Source: "x"},
		{Domain: "alias.example.com", IPs: []netip.Addr{ip("203.0.113.4")}, Source: "x"},
		{Domain: "host.sub.example.com", IPs: []netip.Addr{ip("203.0.113.4")}, Source: "x"},
		{Domain: "shop.child.example.com", IPs: []netip.Addr{ip("203.0.113.4")}, Source: "x"},
		{Domain: "example.org", IPs: []netip.Addr{ip("203.0.113.4")}, Source: "x"},
		{Domain: "notexample.com", IPs: []netip.Addr{ip("203.0.113.4")}, Source: "x"},
	}
	got := AutoRecords(&z, []string{"example.com", "child.example.com"}, targets)
	var lines []string
	for _, r := range got {
		lines = append(lines, r.Name+" "+r.Type+" "+r.Value+" ("+r.Source+")")
		if r.TTL != AutoTTL {
			t.Errorf("ttl %d", r.TTL)
		}
	}
	want := []string{
		"@ A 203.0.113.1 (site blog)",
		"@ AAAA 2001:db8::1 (site blog)",
		"app A 203.0.113.1 (app a, app b)",
		"app A 203.0.113.2 (app a, app b)",
		"txt A 203.0.113.3 (site t)",
		"www A 203.0.113.1 (site blog)",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// The zone stays valid with them.
	for _, r := range got {
		z.Records = append(z.Records, r.Record)
	}
	if err := z.Validate(); err != nil {
		t.Fatal(err)
	}
	// The child zone gets its own domain.
	child := Zone{Name: "child.example.com"}
	if c := AutoRecords(&child, []string{"example.com", "child.example.com"}, targets); len(c) != 1 || c[0].Name != "shop" {
		t.Fatalf("child: %+v", c)
	}
}

func TestPublicAddr(t *testing.T) {
	for v, want := range map[string]bool{
		"8.8.8.8": true, "2a00:1450::1": true, "::ffff:8.8.8.8": true,
		"10.0.0.1": false, "192.168.1.1": false, "172.16.0.1": false, "100.64.0.1": false,
		"127.0.0.1": false, "169.254.1.1": false, "fd00::1": false, "fe80::1": false, "::1": false,
		"192.0.2.1": false, "224.0.0.1": false,
	} {
		if PublicAddr(netip.MustParseAddr(v)) != want {
			t.Errorf("PublicAddr(%s) != %v", v, want)
		}
	}
}
