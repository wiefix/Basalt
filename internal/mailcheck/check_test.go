package mailcheck

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
)

// fakeDNS answers like a recursive resolver from a list of records.
func fakeDNS(t *testing.T, zone string) string {
	t.Helper()
	recs := map[string][]mdns.RR{}
	names := map[string]bool{}
	zp := mdns.NewZoneParser(strings.NewReader(zone), ".", "")
	zp.SetDefaultTTL(300)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		h := rr.Header()
		name := strings.ToLower(h.Name)
		recs[name+" "+mdns.TypeToString[h.Rrtype]] = append(recs[name+" "+mdns.TypeToString[h.Rrtype]], rr)
		// Every ancestor exists (empty non-terminals).
		for n := name; n != ""; n = n[strings.Index(n, ".")+1:] {
			names[n] = true
		}
	}
	if err := zp.Err(); err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &mdns.Server{PacketConn: pc, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		m := new(mdns.Msg)
		m.SetReply(req)
		q := req.Question[0]
		name := strings.ToLower(q.Name)
		switch {
		case recs[name+" "+mdns.TypeToString[q.Qtype]] != nil:
			m.Answer = recs[name+" "+mdns.TypeToString[q.Qtype]]
		case !names[name]:
			m.Rcode = mdns.RcodeNameError
		}
		w.WriteMsg(m)
	})}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

// smtpServer greets every connection with line.
func smtpServer(t *testing.T, line string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(line + "\r\n"))
			c.Close()
		}
	}()
	return l.Addr().String()
}

func closedPort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

const zone = `
mail.example.test.          A     1.2.3.4
mail.example.test.          AAAA  2a01:4f8::1
other.example.test.         AAAA  2a01:4f8::1
4.3.2.1.in-addr.arpa.       PTR   mail.example.test.
1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.f.4.0.1.0.a.2.ip6.arpa. PTR other.example.test.
example.test.               MX    10 mail.example.test.
example.test.               TXT   "v=spf1 mx include:_spf.example.test -all"
_spf.example.test.          TXT   "v=spf1 ip4:5.6.7.8 -all"
v1._domainkey.example.test. TXT   "v=DKIM1; k=ed25519; " "p=KEY1"
_dmarc.example.test.        TXT   "v=DMARC1; p=reject"
4.3.2.1.bl.spamcop.net.     A     127.0.0.2
4.3.2.1.b.barracudacentral.org. A 127.255.255.254

bad.test.                   MX    10 mx.elsewhere.test.
bad.test.                   TXT   "v=spf1 a -all"
bad.test.                   A     9.9.9.9
v1._domainkey.bad.test.     TXT   "v=DKIM1; p=OTHER"
_dmarc.bad.test.            TXT   "v=DMARC1; p=none"

sub.example.test.           MX    20 mail.example.test.
sub.example.test.           MX    10 backup.test.
sub.example.test.           TXT   "v=spf1 redirect=example.test"
`

func run(t *testing.T, c *Checker, req agentproto.MailCheck) map[string]agentproto.MailCheckResult {
	t.Helper()
	out := map[string]agentproto.MailCheckResult{}
	for _, r := range c.Run(context.Background(), req) {
		key := r.Check
		if r.Domain != "" {
			key = r.Domain + " " + key
		} else if r.Target != "" && r.Check != CheckHostname && r.Check != CheckOutbound {
			key += " " + r.Target
		}
		if _, dup := out[key]; dup {
			t.Fatalf("duplicate %s", key)
		}
		out[key] = r
	}
	return out
}

func expect(t *testing.T, got map[string]agentproto.MailCheckResult, key, status, detail string) {
	t.Helper()
	r, ok := got[key]
	if !ok {
		t.Errorf("%s: no result", key)
		return
	}
	if r.Status != status || !strings.Contains(r.Detail, detail) {
		t.Errorf("%s: %s %q, want %s with %q", key, r.Status, r.Detail, status, detail)
	}
}

func TestRun(t *testing.T) {
	resolver := fakeDNS(t, zone)
	c := &Checker{Resolvers: []string{resolver}, SMTPProbes: []string{closedPort(t), smtpServer(t, "220 mx.test ESMTP")},
		Timeout: 10 * time.Second}
	dkim := func(domain, key string) dns.Record {
		return dns.Record{Name: "v1._domainkey." + domain, Type: dns.TypeTXT, Value: "v=DKIM1; k=ed25519; p=" + key}
	}
	req := agentproto.MailCheck{Hostname: "mail.example.test", IPs: []string{"1.2.3.4", "2a01:4f8::1", "1.2.3.4", "192.168.1.1", "junk"},
		Domains: []agentproto.MailCheckDomain{
			{Name: "example.test", Records: []dns.Record{dkim("example.test", "KEY1")}},
			{Name: "bad.test", Records: []dns.Record{dkim("bad.test", "KEY2"), dkim("bad.test", "KEY3"),
				{Name: "_dmarc.bad.test", Type: dns.TypeTXT, Value: "v=DMARC1; p=reject"}}},
			{Name: "sub.example.test"},
			{Name: "missing.test", Records: []dns.Record{
				{Name: "missing.test", Type: dns.TypeTXT, Value: "v=spf1 mx -all"},
				{Name: "_dmarc.missing.test", Type: dns.TypeTXT, Value: "v=DMARC1; p=reject"}}},
		}}
	got := run(t, c, req)
	if len(got) != 2+3+3+4*4 {
		t.Errorf("%d results", len(got))
	}
	expect(t, got, CheckOutbound, OK, "got its greeting")
	expect(t, got, CheckHostname, OK, "1.2.3.4, 2a01:4f8::1")
	expect(t, got, "ptr 1.2.3.4", OK, "1.2.3.4 → mail.example.test → 1.2.3.4")
	expect(t, got, "ptr 2a01:4f8::1", Warn, "PTR of 2a01:4f8::1 is other.example.test")
	expect(t, got, "ptr 192.168.1.1", Unknown, "not a public address")
	expect(t, got, "blocklist 1.2.3.4", Fail, "Listed on bl.spamcop.net (127.0.0.2)")
	expect(t, got, "blocklist 2a01:4f8::1", OK, "Not listed on zen.spamhaus.org.")
	expect(t, got, "blocklist 192.168.1.1", Unknown, "not a public address")

	expect(t, got, "example.test mx", OK, "delivered to mail.example.test")
	expect(t, got, "example.test spf", Fail, "forbids 192.168.1.1") // a private address the policy does not list
	expect(t, got, "example.test dkim", OK, "Keys published: v1")
	expect(t, got, "example.test dmarc", OK, "Policy reject")

	expect(t, got, "bad.test mx", Fail, "goes to mx.elsewhere.test")
	expect(t, got, "bad.test spf", Fail, `"v=spf1 a -all" forbids 1.2.3.4`)
	expect(t, got, "bad.test dkim", Fail, "a different key is published")
	expect(t, got, "bad.test dmarc", Warn, "Policy none")

	expect(t, got, "sub.example.test mx", Warn, "backup.test has priority")
	expect(t, got, "sub.example.test dmarc", OK, "(from example.test)")
	expect(t, got, "sub.example.test dkim", Unknown, "not created DKIM keys")

	expect(t, got, "missing.test mx", Fail, "does not exist in DNS")
	expect(t, got, "missing.test spf", Fail, `Add TXT "v=spf1 mx -all"`)
	expect(t, got, "missing.test dmarc", Fail, `Add TXT "v=DMARC1; p=reject" at _dmarc.missing.test`)

	// Only public addresses: SPF passes for them through mx and redirect.
	req.IPs = []string{"1.2.3.4", "2a01:4f8::1"}
	got = run(t, c, req)
	expect(t, got, "example.test spf", OK, "allows this server")
	expect(t, got, "sub.example.test spf", OK, "allows this server")

	// Outgoing port 25 blocked; no address known.
	c.SMTPProbes = []string{closedPort(t)}
	req.IPs = nil
	got = run(t, c, req)
	expect(t, got, CheckOutbound, Fail, "Many providers block outgoing port 25")
	expect(t, got, CheckHostname, Warn, "public address is unknown")
	expect(t, got, "example.test spf", Unknown, "public address is unknown")

	// A greeting that refuses.
	c.SMTPProbes = []string{smtpServer(t, "554 go away")}
	expect(t, run(t, c, req), CheckOutbound, Fail, "refused: 554 go away")

	// The host name points elsewhere.
	req.Hostname, req.IPs = "bad.test", []string{"1.2.3.4"}
	expect(t, run(t, c, req), CheckHostname, Fail, "resolves to 9.9.9.9, not to this server (1.2.3.4)")

	// No resolver answers.
	c.Resolvers, c.Timeout = []string{closedPort(t)}, 3*time.Second
	got = run(t, c, agentproto.MailCheck{Hostname: "mail.example.test", IPs: []string{"1.2.3.4"}})
	expect(t, got, CheckHostname, Unknown, "DNS query for mail.example.test failed")
}

func TestSPF(t *testing.T) {
	srv := fakeDNS(t, `
a.test.     TXT "v=spf1 a/24 a:host.a.test//64 -all"
a.test.     A   10.0.0.1
host.a.test. AAAA 2001:db8:1::1
loop.test.  TXT "v=spf1 include:loop.test -all"
macro.test. TXT "v=spf1 exists:%{i}.x.test -all"
two.test.   TXT "v=spf1 -all"
two.test.   TXT "v=spf1 +all"
soft.test.  TXT "v=spf1 ~all"
nopol.test. TXT "something else"
inc.test.   TXT "v=spf1 include:nopol.test -all"
red.test.   TXT "v=spf1 redirect=nopol.test"
void.test.  TXT "v=spf1 a:n1.test a:n2.test a:n3.test -all"
ex.test.    TXT "v=SPF1 exists:ok.test ?all"
ok.test.    A   127.0.0.2
bad.test.   TXT "v=spf1 ip4:300.1.1.1 -all"
spf2.test.  TXT "v=spf10 -all"
`)
	r := &resolver{servers: []string{srv}, timeout: 2 * time.Second}
	for _, tc := range []struct {
		domain, ip, result, err string
	}{
		{"a.test", "10.0.0.200", spfPass, ""},
		{"a.test", "10.0.1.1", spfFail, ""},
		{"a.test", "2001:db8:1::ffff", spfPass, ""},
		{"a.test", "2001:db8:2::1", spfFail, ""},
		{"loop.test", "1.1.1.1", spfPermError, "more than 10 DNS lookups"},
		{"macro.test", "1.1.1.1", spfPermError, "macros"},
		{"two.test", "1.1.1.1", spfPermError, "several SPF records"},
		{"soft.test", "1.1.1.1", spfSoftFail, ""},
		{"nopol.test", "1.1.1.1", spfNone, ""},
		{"spf2.test", "1.1.1.1", spfNone, ""},
		{"nothere.test", "1.1.1.1", spfNone, ""},
		{"inc.test", "1.1.1.1", spfPermError, "include:nopol.test has no valid SPF policy"},
		{"red.test", "1.1.1.1", spfPermError, "redirect to nopol.test finds no policy"},
		{"void.test", "1.1.1.1", spfPermError, "more than 2 lookups that find nothing"},
		{"ex.test", "1.1.1.1", spfPass, ""},
		{"bad.test", "1.1.1.1", spfPermError, "invalid SPF term ip4:300.1.1.1"},
	} {
		e := &spfEval{r: r, ip: net.ParseIP(tc.ip)}
		res, err := e.check(context.Background(), tc.domain)
		if res != tc.result || (tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err))) {
			t.Errorf("%s %s: %s %v, want %s %q", tc.domain, tc.ip, res, err, tc.result, tc.err)
		}
	}
}

func TestParsers(t *testing.T) {
	for in, want := range map[string]bool{"v=DMARC1; p=reject": true, "v=DMARC1": true, " v=dmarc1 ;p=none": true, "v=DMARC10; p=x": false, "p=reject": false} {
		if isDMARC(in) != want {
			t.Errorf("isDMARC(%q) = %v", in, !want)
		}
	}
	if k := dkimKey("v=DKIM1; k=rsa; p=AB CD\tEF "); k != "ABCDEF" {
		t.Errorf("dkimKey %q", k)
	}
	if d, v4, v6, ok := parseDomainCIDR(":x.test/24//48", "d.test"); !ok || d != "x.test" || v4 != 24 || v6 != 48 {
		t.Errorf("%s %d %d %v", d, v4, v6, ok)
	}
	for _, bad := range []string{"/33", "//129", ":", "x.test", "/a"} {
		if _, _, _, ok := parseDomainCIDR(bad, "d.test"); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
