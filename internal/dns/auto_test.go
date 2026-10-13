package dns

import (
	"net/netip"
	"strconv"
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

func TestChallengeRecords(t *testing.T) {
	z := Zone{Name: "example.com", Records: []Record{
		rec("_acme-challenge.delegated", TypeCNAME, "x.acme-dns.example.org"),
		rec("sub", TypeNS, "ns.example.org"),
	}}
	got := ChallengeRecords(&z, []string{"example.com", "child.example.com"}, []Challenge{
		{"_acme-challenge.example.com", "token-1"},
		{"_acme-challenge.example.com", "token-2"}, // wildcard and apex at once
		{"_acme-challenge.example.com", "token-1"}, // asked twice
		{"_acme-challenge.www.example.com", "token-3"},
		{"_acme-challenge.delegated.example.com", "x"},
		{"_acme-challenge.sub.example.com", "x"},
		{"_acme-challenge.child.example.com", "x"},
		{"_acme-challenge.example.org", "x"},
	})
	var lines []string
	for _, r := range got {
		lines = append(lines, r.Name+" "+r.Type+" "+r.Value)
		z.Records = append(z.Records, r.Record)
	}
	if strings.Join(lines, "|") != "_acme-challenge TXT token-1|_acme-challenge TXT token-2|_acme-challenge.www TXT token-3" {
		t.Fatalf("%v", lines)
	}
	if err := z.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRecords(t *testing.T) {
	z := Zone{Name: "example.com", Records: []Record{
		rec("@", TypeTXT, "v=spf1 include:_spf.google.com -all"), // the user's SPF stays
		rec("autoconfig", TypeA, "192.0.2.5"),
		rec("legacy", TypeCNAME, "elsewhere.example.org"),
	}}
	mx := rec("example.com", TypeMX, "mail.example.com")
	mx.Priority = 10
	srv := rec("_imaps._tcp.example.com", TypeSRV, "mail.example.com")
	srv.Priority, srv.Weight, srv.Port = 0, 1, 993
	want := []Record{
		mx,
		rec("example.com", TypeTXT, "v=spf1 mx -all"),
		rec("_dmarc.example.com", TypeTXT, "v=DMARC1; p=reject"),
		rec("v1-rsa-20261011._domainkey.example.com", TypeTXT, "v=DKIM1; k=rsa; p=MIIB"),
		srv,
		rec("autoconfig.example.com", TypeCNAME, "mail.example.com"),   // the user's A wins
		rec("mta-sts.example.com", TypeCNAME, "mail.example.com"),      // an automatic A is there
		rec("autodiscover.example.com", TypeCNAME, "mail.example.com"), // free
		rec("legacy.example.com", TypeTXT, "v=spf1 -all"),              // beside a CNAME
		rec("other.example.org", TypeTXT, "x"),                         // not this zone
	}
	taken := []AutoRecord{{Record: rec("mta-sts", TypeA, "192.0.2.1")}}
	got := ServiceRecords(&z, []string{"example.com"}, want, "mail", taken)
	var lines []string
	for _, r := range got {
		lines = append(lines, r.Name+" "+r.Type)
		z.Records = append(z.Records, r.Record)
	}
	if strings.Join(lines, "|") != "@ MX|_dmarc TXT|v1-rsa-20261011._domainkey TXT|_imaps._tcp SRV|autodiscover CNAME" {
		t.Fatalf("%v", lines)
	}
	if err := z.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAutoRecordsFollowChecks(t *testing.T) {
	ip := netip.MustParseAddr
	z := Zone{Name: "example.com"}
	targets := []AutoTarget{
		// An app on two servers, each copy with its health check.
		{Domain: "app.example.com", IPs: []netip.Addr{ip("203.0.113.1")}, Source: "app a", HealthCheck: 7},
		{Domain: "app.example.com", IPs: []netip.Addr{ip("203.0.113.2")}, Source: "app a", HealthCheck: 8},
		// An address also used by something without a check is always served.
		{Domain: "mixed.example.com", IPs: []netip.Addr{ip("203.0.113.1")}, Source: "app b", HealthCheck: 7},
		{Domain: "mixed.example.com", IPs: []netip.Addr{ip("203.0.113.1")}, Source: "route"},
	}
	var got []string
	for _, r := range AutoRecords(&z, []string{"example.com"}, targets) {
		got = append(got, r.Name+" "+r.Value+" ttl="+strconv.Itoa(int(r.TTL))+" check="+strconv.Itoa(int(r.HealthCheck)))
	}
	want := "app 203.0.113.1 ttl=60 check=7, app 203.0.113.2 ttl=60 check=8, mixed 203.0.113.1 ttl=300 check=0"
	if strings.Join(got, ", ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, ", "), want)
	}
	// With check 7 down only the other copy answers.
	recs := AutoRecords(&z, []string{"example.com"}, targets)
	for _, r := range recs {
		z.Records = append(z.Records, r.Record)
	}
	Failover(&z, func(id int64) bool { return id == 7 })
	var served []string
	for _, r := range z.Records {
		served = append(served, r.Name+" "+r.Value)
	}
	if strings.Join(served, ", ") != "app 203.0.113.2, mixed 203.0.113.1" {
		t.Fatalf("served %v", served)
	}
}
