package dns

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func rec(name, typ, value string) Record {
	r := Record{Name: name, Type: typ, Value: value}
	r.Normalize()
	return r
}

func testZones() []Zone {
	mx := rec("@", TypeMX, "mail.example.com")
	mx.Priority = 10
	srv := rec("_sip._tcp", TypeSRV, "sip.example.com")
	srv.Priority, srv.Weight, srv.Port = 1, 2, 5060
	caa := rec("@", TypeCAA, "letsencrypt.org")
	caa.Tag = "issue"
	return []Zone{{
		Name: "example.com", Serial: 2026101100,
		Records: []Record{
			rec("@", TypeNS, "ns1.example.com."),
			rec("@", TypeNS, "ns2.example.net"),
			rec("@", TypeA, "192.0.2.1"),
			rec("@", TypeAAAA, "2001:db8::1"),
			mx, srv, caa,
			rec("@", TypeTXT, `v=spf1 -all "quoted" \ back`+strings.Repeat("x", 300)),
			rec("ns1", TypeA, "192.0.2.53"),
			rec("www", TypeCNAME, "example.com"),
			rec("ext", TypeCNAME, "target.example.org"),
			rec("loop1", TypeCNAME, "loop2.example.com"),
			rec("loop2", TypeCNAME, "loop1.example.com"),
			rec("a.b.deep", TypeA, "192.0.2.7"),
			rec("*.wild", TypeA, "192.0.2.8"),
			rec("sub", TypeNS, "ns.sub.example.com"),
			rec("ns.sub", TypeA, "192.0.2.9"),
			rec("app", TypeALIAS, "lb.example.com"),
			rec("lb", TypeA, "192.0.2.10"),
			rec("lb", TypeA, "192.0.2.11"),
			rec("outside", TypeALIAS, "cdn.example.org"),
		},
	}, {
		Name: "child.example.com", Serial: 1,
		Records: []Record{rec("@", TypeA, "198.51.100.1")},
	}}
}

type fakeResolver struct {
	calls int
	err   error
}

func (f *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if host != "cdn.example.org." {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if network == "ip6" {
		return []netip.Addr{netip.MustParseAddr("2001:db8::99")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("203.0.113.99")}, nil
}

func newTestServer(t *testing.T) (*Server, *fakeResolver) {
	t.Helper()
	res := &fakeResolver{}
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), res)
	if err := s.SetZones(testZones()); err != nil {
		t.Fatal(err)
	}
	return s, res
}

func query(s *Server, name string, qtype uint16) *mdns.Msg {
	m := new(mdns.Msg)
	m.SetQuestion(name, qtype)
	return s.answer(m)
}

func answers(m *mdns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		out = append(out, strings.ReplaceAll(rr.String(), "\t", " "))
	}
	return out
}

func TestAnswers(t *testing.T) {
	s, _ := newTestServer(t)
	for _, tc := range []struct {
		name  string
		qtype uint16
		rcode int
		aa    bool
		want  []string // substrings of answer records, in order
		ns    string   // substring of the first authority record
	}{
		{"example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"example.com. 3600 IN A 192.0.2.1"}, ""},
		{"EXAMPLE.com.", mdns.TypeAAAA, mdns.RcodeSuccess, true, []string{"EXAMPLE.com. 3600 IN AAAA 2001:db8::1"}, ""},
		{"example.com.", mdns.TypeMX, mdns.RcodeSuccess, true, []string{"MX 10 mail.example.com."}, ""},
		{"_sip._tcp.example.com.", mdns.TypeSRV, mdns.RcodeSuccess, true, []string{"SRV 1 2 5060 sip.example.com."}, ""},
		{"example.com.", mdns.TypeCAA, mdns.RcodeSuccess, true, []string{`CAA 0 issue "letsencrypt.org"`}, ""},
		{"example.com.", mdns.TypeSOA, mdns.RcodeSuccess, true, []string{"SOA ns1.example.com. hostmaster.example.com. 2026101100 3600 600 1209600 300"}, ""},
		{"example.com.", mdns.TypeNS, mdns.RcodeSuccess, true, []string{"NS ns1.example.com.", "NS ns2.example.net."}, ""},
		// NODATA and NXDOMAIN carry the SOA with the negative TTL.
		{"example.com.", mdns.TypePTR, mdns.RcodeSuccess, true, nil, "example.com. 300 IN SOA"},
		{"nope.example.com.", mdns.TypeA, mdns.RcodeNameError, true, nil, "example.com. 300 IN SOA"},
		// Empty non-terminal: exists, no data.
		{"deep.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, nil, "SOA"},
		{"a.b.deep.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"192.0.2.7"}, ""},
		// CNAME is followed inside our zones; outside it is left to the resolver.
		{"www.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"www.example.com. 3600 IN CNAME example.com.", "example.com. 3600 IN A 192.0.2.1"}, ""},
		{"www.example.com.", mdns.TypeCNAME, mdns.RcodeSuccess, true, []string{"CNAME example.com."}, ""},
		{"ext.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"CNAME target.example.org."}, ""},
		// A loop stops after a bounded number of steps.
		{"loop1.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"CNAME loop2", "CNAME loop1", "CNAME loop2", "CNAME loop1", "CNAME loop2", "CNAME loop1", "CNAME loop2", "CNAME loop1"}, ""},
		// Wildcards answer with the asked name, for any depth below.
		{"x.wild.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"x.wild.example.com. 3600 IN A 192.0.2.8"}, ""},
		{"y.x.wild.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"y.x.wild.example.com. 3600 IN A 192.0.2.8"}, ""},
		{"x.wild.example.com.", mdns.TypeMX, mdns.RcodeSuccess, true, nil, "SOA"},
		{"wild.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, nil, "SOA"}, // the parent itself is not covered
		// Delegation: a referral without the AA bit, with glue.
		{"host.sub.example.com.", mdns.TypeA, mdns.RcodeSuccess, false, nil, "sub.example.com. 3600 IN NS ns.sub.example.com."},
		{"sub.example.com.", mdns.TypeDS, mdns.RcodeSuccess, true, nil, "SOA"},
		// ALIAS: flattened from our zone or from the resolver.
		{"app.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"app.example.com. 3600 IN A 192.0.2.10", "app.example.com. 3600 IN A 192.0.2.11"}, ""},
		{"app.example.com.", mdns.TypeAAAA, mdns.RcodeSuccess, true, nil, "SOA"},
		{"outside.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"outside.example.com. 3600 IN A 203.0.113.99"}, ""},
		{"outside.example.com.", mdns.TypeAAAA, mdns.RcodeSuccess, true, []string{"AAAA 2001:db8::99"}, ""},
		// The more specific zone wins.
		{"child.example.com.", mdns.TypeA, mdns.RcodeSuccess, true, []string{"198.51.100.1"}, ""},
		// Not ours: refused, never recursed.
		{"example.org.", mdns.TypeA, mdns.RcodeRefused, false, nil, ""},
		{"com.", mdns.TypeNS, mdns.RcodeRefused, false, nil, ""},
		{"example.com.", mdns.TypeAXFR, mdns.RcodeRefused, false, nil, ""},
		{"example.com.", mdns.TypeANY, mdns.RcodeSuccess, true, []string{"HINFO \"RFC8482\""}, ""},
	} {
		m := query(s, tc.name, tc.qtype)
		got := answers(m)
		label := tc.name + " " + mdns.TypeToString[tc.qtype]
		if m.Rcode != tc.rcode || m.Authoritative != tc.aa || m.RecursionAvailable {
			t.Errorf("%s: rcode %s aa %v ra %v", label, mdns.RcodeToString[m.Rcode], m.Authoritative, m.RecursionAvailable)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: answers %q, want %q", label, got, tc.want)
			continue
		}
		for i, w := range tc.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: answer %d = %q, want %q", label, i, got[i], w)
			}
		}
		if tc.ns != "" && (len(m.Ns) == 0 || !strings.Contains(strings.ReplaceAll(m.Ns[0].String(), "\t", " "), tc.ns)) {
			t.Errorf("%s: authority %v, want %q", label, m.Ns, tc.ns)
		}
	}
	// Glue for the delegated nameserver.
	m := query(s, "host.sub.example.com.", mdns.TypeA)
	if len(m.Extra) != 1 || !strings.Contains(m.Extra[0].String(), "192.0.2.9") {
		t.Errorf("glue: %v", m.Extra)
	}
}

func TestTXTRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	m := query(s, "example.com.", mdns.TypeTXT)
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	var back mdns.Msg
	if err := back.Unpack(b); err != nil {
		t.Fatal(err)
	}
	txt := back.Answer[0].(*mdns.TXT).Txt
	if len(txt) != 2 || len(txt[0]) < 255 {
		t.Fatalf("TXT not split into 255-byte strings: %d parts", len(txt))
	}
	// Unpack returns presentation form; undo it to compare bytes.
	got := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(strings.Join(txt, ""))
	if want := `v=spf1 -all "quoted" \ back` + strings.Repeat("x", 300); got != want {
		t.Fatalf("TXT changed on the wire:\n%q\n%q", got, want)
	}
}

func TestAliasResolverCache(t *testing.T) {
	s, res := newTestServer(t)
	query(s, "outside.example.com.", mdns.TypeA)
	query(s, "outside.example.com.", mdns.TypeA)
	if res.calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (cached)", res.calls)
	}
	// A failing resolver with nothing cached is a SERVFAIL, not a lie.
	s2 := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeResolver{err: context.DeadlineExceeded})
	if err := s2.SetZones(testZones()); err != nil {
		t.Fatal(err)
	}
	if m := query(s2, "outside.example.com.", mdns.TypeA); m.Rcode != mdns.RcodeServerFailure {
		t.Fatalf("rcode %s", mdns.RcodeToString[m.Rcode])
	}
}

func TestProtocolEdges(t *testing.T) {
	s, _ := newTestServer(t)
	m := new(mdns.Msg)
	m.SetQuestion("example.com.", mdns.TypeA)
	m.Question[0].Qclass = mdns.ClassCHAOS
	if r := s.answer(m); r.Rcode != mdns.RcodeRefused {
		t.Errorf("CHAOS: %s", mdns.RcodeToString[r.Rcode])
	}
	m = new(mdns.Msg)
	m.SetQuestion("example.com.", mdns.TypeA)
	m.SetEdns0(4096, false)
	m.IsEdns0().SetVersion(1)
	if r := s.answer(m); r.Rcode != mdns.RcodeBadVers {
		t.Errorf("EDNS version 1: %s", mdns.RcodeToString[r.Rcode])
	}
	m = new(mdns.Msg)
	m.SetQuestion("example.com.", mdns.TypeA)
	m.RecursionDesired = true
	if r := s.answer(m); r.RecursionAvailable || !r.RecursionDesired {
		t.Errorf("RD/RA: %v %v", r.RecursionDesired, r.RecursionAvailable)
	}
}

func TestServeUDPAndTCP(t *testing.T) {
	s, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, k) }()
	addr := k.Addrs()[0]

	// A big answer over UDP without EDNS is truncated to 512 bytes; the
	// same query over TCP is complete.
	big := testZones()
	for i := range 40 {
		big[0].Records = append(big[0].Records, rec("big", TypeA, netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}).String()))
	}
	if err := s.SetZones(big); err != nil {
		t.Fatal(err)
	}
	q := new(mdns.Msg)
	q.SetQuestion("big.example.com.", mdns.TypeA)
	c := &mdns.Client{Net: "udp", Timeout: 2 * time.Second}
	r, _, err := c.Exchange(q, addr)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Answer) >= 40 {
		t.Fatalf("udp: truncated %v, %d answers", r.Truncated, len(r.Answer))
	}
	c.Net = "tcp"
	r, _, err = c.Exchange(q, addr)
	if err != nil {
		t.Fatal(err)
	}
	if r.Truncated || len(r.Answer) != 40 {
		t.Fatalf("tcp: truncated %v, %d answers", r.Truncated, len(r.Answer))
	}
	// With EDNS the UDP limit is 1232 bytes: still truncated here, but the
	// OPT record comes back.
	q.SetEdns0(4096, false)
	c.Net = "udp"
	r, _, err = c.Exchange(q, addr)
	if err != nil {
		t.Fatal(err)
	}
	if r.IsEdns0() == nil || r.IsEdns0().UDPSize() != maxUDPSize {
		t.Fatalf("edns: %v", r.IsEdns0())
	}

	// Zones can be replaced while serving.
	if err := s.SetZones(nil); err != nil {
		t.Fatal(err)
	}
	r, _, err = c.Exchange(q, addr)
	if err != nil || r.Rcode != mdns.RcodeRefused {
		t.Fatalf("after removing zones: %v %v", r, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestUDPRateLimit(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Unix(1000, 0)
	s.limiter = newPrefixLimiter(udpRate, udpBurst, func() time.Time { return now })
	q := new(mdns.Msg)
	q.SetQuestion("example.com.", mdns.TypeA)
	from := &net.UDPAddr{IP: net.ParseIP("203.0.113.5"), Port: 5353}
	neighbour := &net.UDPAddr{IP: net.ParseIP("203.0.113.77"), Port: 5353}
	other := &net.UDPAddr{IP: net.ParseIP("198.51.100.5"), Port: 5353}
	full, truncated, dropped := 0, 0, 0
	for range 300 {
		w := &recorder{remote: from}
		s.ServeDNS(w, q)
		switch {
		case w.msg == nil:
			dropped++
		case w.msg.Truncated && len(w.msg.Answer) == 0:
			truncated++
		default:
			full++
		}
	}
	if full != udpBurst || truncated == 0 || dropped == 0 || truncated+dropped != 300-udpBurst {
		t.Fatalf("full %d truncated %d dropped %d", full, truncated, dropped)
	}
	serve := func(addr net.Addr) *mdns.Msg {
		w := &recorder{remote: addr}
		s.ServeDNS(w, q)
		return w.msg
	}
	// The same /24 shares the budget; another network does not.
	if m := serve(neighbour); m != nil && !m.Truncated {
		t.Fatal("neighbour in the same /24 not limited")
	}
	if m := serve(other); m == nil || m.Truncated {
		t.Fatal("other network limited")
	}
	// Tokens come back with time.
	now = now.Add(time.Second)
	if m := serve(from); m == nil || m.Truncated {
		t.Fatal("no refill")
	}
	// TCP is not rate limited.
	if m := serve(&net.TCPAddr{IP: from.IP, Port: 1}); m == nil || m.Truncated {
		t.Fatal("tcp limited")
	}
}

func TestTCPConnectionLimit(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	l := newLimitListener(inner, 3, 2)
	accepted := make(chan net.Conn, 10)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	a1, a2 := <-accepted, <-accepted
	// The third connection from the same address is closed at once.
	clients[2].SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := clients[2].Read(make([]byte, 1)); err == nil {
		t.Fatal("third connection from one IP served")
	}
	select {
	case c := <-accepted:
		t.Fatalf("unexpected accept %v", c.RemoteAddr())
	case <-time.After(100 * time.Millisecond):
	}
	// Closing one frees a slot.
	a1.Close()
	c, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case a3 := <-accepted:
		a3.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("slot not freed")
	}
	a2.Close()
	for _, c := range clients {
		c.Close()
	}
}

type recorder struct {
	mdns.ResponseWriter
	remote net.Addr
	msg    *mdns.Msg
}

func (r *recorder) RemoteAddr() net.Addr       { return r.remote }
func (r *recorder) WriteMsg(m *mdns.Msg) error { r.msg = m; return nil }

func TestValidation(t *testing.T) {
	ok := Zone{Name: "example.com", Records: []Record{rec("@", TypeA, "192.0.2.1")}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		zone string
		recs []Record
	}{
		{"com", nil},
		{"exa mple.com", nil},
		{"-x.com", nil},
		{"under_score.com", nil},
		{"example.com", []Record{rec("@", TypeA, "2001:db8::1")}},
		{"example.com", []Record{rec("@", TypeAAAA, "192.0.2.1")}},
		{"example.com", []Record{rec("@", TypeAAAA, "::ffff:192.0.2.1")}},
		{"example.com", []Record{rec("@", TypeCNAME, "other.example.org")}},
		{"example.com", []Record{rec("www", TypeCNAME, "a.example.org"), rec("www", TypeTXT, "x")}},
		{"example.com", []Record{rec("www", TypeCNAME, "www.example.com")}},
		{"example.com", []Record{rec("@", TypeALIAS, "a.example.org"), rec("@", TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec("@", TypeALIAS, "a.example.org"), rec("@", TypeALIAS, "b.example.org")}},
		{"example.com", []Record{rec("@", TypeA, "192.0.2.1"), rec("@", TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec("a*", TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec("x.*", TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec("a..b", TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec("bad name", TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec(strings.Repeat("a", 64), TypeA, "192.0.2.1")}},
		{"example.com", []Record{rec("@", TypeNS, "localhost")}},
		{"example.com", []Record{rec("@", TypeNS, ".")}},
		{"example.com", []Record{rec("@", TypeMX, "mail;rm.example.com")}},
		{"example.com", []Record{rec("@", TypeTXT, "line\nbreak")}},
		{"example.com", []Record{rec("@", TypeTXT, strings.Repeat("x", MaxTXTLength+1))}},
		{"example.com", []Record{rec("@", TypeCAA, "x")}}, // no tag
		{"example.com", []Record{rec("@", "SOA", "x")}},
		{"example.com", []Record{rec("@", "HINFO", "x")}},
		{"example.com", []Record{rec("@", TypeSRV, "sip.example.com")}},
		{"example.com", []Record{{Name: "@", Type: TypeA, TTL: 10, Value: "192.0.2.1"}}},
	} {
		z := Zone{Name: bad.zone, Records: bad.recs}
		if err := z.Validate(); err == nil {
			t.Errorf("accepted zone %q %+v", bad.zone, bad.recs)
		}
	}
	nullMX := rec("@", TypeMX, ".")
	caa := rec("@", TypeCAA, "mailto:sec@example.com")
	caa.Tag, caa.Flags = "iodef", 128
	z := Zone{Name: "xn--e1afmkfd.xn--p1ai", Records: []Record{nullMX, caa, rec("_dmarc", TypeTXT, "v=DMARC1; p=none"),
		rec("*", TypeA, "192.0.2.1"), rec("1", TypePTR, "host.example.com")}}
	if err := z.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Check([]Zone{ok, ok}); err == nil {
		t.Fatal("duplicate zone accepted")
	}
}

func TestNormalize(t *testing.T) {
	r := Record{Name: " WWW. ", Type: "cname", Value: "Target.Example.ORG.", Priority: 5, Tag: "x"}
	r.Normalize()
	if r.Name != "www" || r.Type != TypeCNAME || r.Value != "target.example.org" || r.Priority != 0 || r.Tag != "" || r.TTL != DefaultTTL {
		t.Fatalf("%+v", r)
	}
	r = Record{Name: "", Type: "AAAA", Value: "2001:DB8:0::1"}
	r.Normalize()
	if r.Name != "@" || r.Value != "2001:db8::1" {
		t.Fatalf("%+v", r)
	}
	if NormalizeZoneName(" Example.COM. ") != "example.com" {
		t.Fatal("zone name")
	}
}
