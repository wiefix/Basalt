package e2e

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"
	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/proxy/caddyrun"
)

// dnsQuery asks the server at addr over UDP and returns the rcode and the
// answer records in presentation form.
func dnsQuery(t *testing.T, addr, name string, qtype uint16) (int, []string) {
	t.Helper()
	q := new(mdns.Msg)
	q.SetQuestion(name, qtype)
	c := &mdns.Client{Net: "udp", Timeout: 2 * time.Second}
	r, _, err := c.Exchange(q, addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	var out []string
	for _, rr := range r.Answer {
		out = append(out, strings.ReplaceAll(rr.String(), "\t", " "))
	}
	return r.Rcode, out
}

func TestDNS(t *testing.T) {
	p := startPanel(t)

	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/dns/nameservers", map[string]any{"nameservers": []string{"not a host"}})
	r := p.call(200, "PUT", "/api/v1/dns/nameservers", map[string]any{"nameservers": []string{"NS1.Example.net.", "ns2.example.net"}})
	if jsonString(r["nameservers"]) != `["ns1.example.net","ns2.example.net"]` {
		t.Fatalf("nameservers = %v", r)
	}

	dnsHost := startHostWith(t, p, "", func(c *agent.Config) { c.DNSListen = "127.0.0.1:0" })
	plain := startHost(t, p)

	var addr string
	overview := p.call(200, "GET", "/api/v1/dns", nil)
	for _, s := range overview["servers"].([]any) {
		s := s.(map[string]any)
		port := int(s["port"].(float64))
		switch jsonID(s["server_id"]) {
		case dnsHost.id:
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			if port == 0 {
				t.Fatalf("dns host reports no port: %v", s)
			}
		case plain.id:
			if port != 0 {
				t.Fatalf("plain host reports a dns port: %v", s)
			}
		}
	}

	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]string{"name": "com"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]string{"name": "bad_zone.test"})
	r = p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "Example.TEST."})
	zone := r["zone"].(map[string]any)
	if zone["name"] != "example.test" || zone["records"].(float64) != 2 {
		t.Fatalf("zone = %v", zone)
	}
	zid := jsonID(zone["id"])
	p.call(http.StatusConflict, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})
	base := "/api/v1/dns/zones/" + zid

	// New zones get NS records for the configured nameservers; the SOA names the first.
	eventually(t, "zone served", func() bool {
		rcode, ans := dnsQuery(t, addr, "example.test.", mdns.TypeSOA)
		return rcode == mdns.RcodeSuccess && len(ans) == 1 && strings.Contains(ans[0], "SOA ns1.example.net. hostmaster.example.test.")
	})
	if _, ans := dnsQuery(t, addr, "example.test.", mdns.TypeNS); len(ans) != 2 {
		t.Fatalf("NS = %v", ans)
	}

	add := func(want int, rec map[string]any) map[string]any {
		t.Helper()
		return p.call(want, "POST", base+"/records", rec)
	}
	a := add(http.StatusCreated, map[string]any{"name": "@", "type": "A", "value": "192.0.2.10", "ttl": 300})["record"].(map[string]any)
	add(http.StatusCreated, map[string]any{"name": "www", "type": "cname", "value": "example.test."})
	add(http.StatusCreated, map[string]any{"name": "@", "type": "MX", "value": "mail.example.test", "priority": 10})
	add(http.StatusCreated, map[string]any{"name": "@", "type": "TXT", "value": "v=spf1 mx -all"})
	add(http.StatusCreated, map[string]any{"name": "@", "type": "CAA", "tag": "issue", "value": "letsencrypt.org"})
	// Invalid records and combinations are refused with a reason.
	for _, bad := range []map[string]any{
		{"name": "@", "type": "CNAME", "value": "other.example.org"},
		{"name": "www", "type": "A", "value": "192.0.2.1"}, // next to the CNAME
		{"name": "x", "type": "A", "value": "2001:db8::1"},
		{"name": "x", "type": "A", "value": "192.0.2.1", "ttl": 5},
		{"name": "x y", "type": "A", "value": "192.0.2.1"},
		{"name": "x", "type": "SOA", "value": "x"},
		{"name": "@", "type": "A", "value": "192.0.2.10"}, // duplicate
	} {
		add(http.StatusUnprocessableEntity, bad)
	}
	add(http.StatusBadRequest, map[string]any{"name": "x", "type": "A", "value": "192.0.2.1", "id": 5})

	eventually(t, "records served", func() bool {
		_, ans := dnsQuery(t, addr, "WWW.example.test.", mdns.TypeA)
		return len(ans) == 2 && strings.Contains(ans[0], "WWW.example.test. 3600 IN CNAME example.test.") &&
			strings.Contains(ans[1], "example.test. 300 IN A 192.0.2.10")
	})
	if _, ans := dnsQuery(t, addr, "example.test.", mdns.TypeMX); len(ans) != 1 || !strings.Contains(ans[0], "MX 10 mail.example.test.") {
		t.Fatalf("MX = %v", ans)
	}
	if rcode, _ := dnsQuery(t, addr, "nope.example.test.", mdns.TypeA); rcode != mdns.RcodeNameError {
		t.Fatalf("missing name: %s", mdns.RcodeToString[rcode])
	}
	if rcode, _ := dnsQuery(t, addr, "example.org.", mdns.TypeA); rcode != mdns.RcodeRefused {
		t.Fatalf("foreign zone: %s", mdns.RcodeToString[rcode])
	}

	// Editing a record changes the answer and the serial.
	serial := p.call(200, "GET", base, nil)["zone"].(map[string]any)["serial"].(float64)
	p.call(200, "PUT", base+"/records/"+jsonID(a["id"]), map[string]any{"name": "@", "type": "A", "value": "192.0.2.20"})
	eventually(t, "edit served", func() bool {
		_, ans := dnsQuery(t, addr, "example.test.", mdns.TypeA)
		return len(ans) == 1 && strings.Contains(ans[0], "3600 IN A 192.0.2.20")
	})
	got := p.call(200, "GET", base, nil)
	if got["zone"].(map[string]any)["serial"].(float64) <= serial || len(got["records"].([]any)) != 7 {
		t.Fatalf("after edit: %v", got)
	}

	// Every agent receives the zones, also those that do not serve DNS.
	eventually(t, "all servers in sync", func() bool {
		for _, s := range p.call(200, "GET", "/api/v1/dns", nil)["servers"].([]any) {
			if s.(map[string]any)["in_sync"] != true {
				return false
			}
		}
		return true
	})

	// Viewers read, only admins change.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "m@example.com", "name": "M", "password": "member password 1", "role": "member",
	})
	member := *p
	member.hc = &http.Client{Jar: newJar()}
	member.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "member password 1"})
	member.call(200, "GET", base, nil)
	member.call(http.StatusForbidden, "POST", base+"/records", map[string]any{"name": "x", "type": "A", "value": "192.0.2.1"})
	member.call(http.StatusForbidden, "DELETE", base, nil)
	member.call(http.StatusForbidden, "PUT", "/api/v1/dns/nameservers", map[string]any{"nameservers": []string{}})

	p.call(http.StatusNoContent, "DELETE", base+"/records/"+jsonID(a["id"]), nil)
	p.call(http.StatusNotFound, "DELETE", base+"/records/"+jsonID(a["id"]), nil)
	p.call(http.StatusNoContent, "DELETE", base, nil)
	eventually(t, "zone removed", func() bool {
		rcode, _ := dnsQuery(t, addr, "example.test.", mdns.TypeA)
		return rcode == mdns.RcodeRefused
	})
	var audit int
	p.db.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action LIKE 'dns%'`).Scan(&audit)
	if audit < 9 {
		t.Fatalf("audit entries: %d", audit)
	}
}

func TestAutoDNS(t *testing.T) {
	p := startPanel(t)
	h := startHostWith(t, p, "", func(c *agent.Config) { c.DNSListen = "127.0.0.1:0" })
	var addr string
	for _, s := range p.call(200, "GET", "/api/v1/dns", nil)["servers"].([]any) {
		s := s.(map[string]any)
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(s["port"].(float64))))
	}
	srvPath := "/api/v1/servers/" + h.id
	p.call(http.StatusUnprocessableEntity, "PUT", srvPath+"/public-ips", map[string]any{"ips": []string{"not-an-ip"}})
	r := p.call(200, "PUT", srvPath+"/public-ips", map[string]any{"ips": []string{"203.0.113.10", "2001:db8::10"}})
	if jsonString(r["server"].(map[string]any)["dns_ips"]) != `["203.0.113.10","2001:db8::10"]` {
		t.Fatalf("server = %v", r)
	}

	zid := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})["zone"].(map[string]any)["id"])
	base := "/api/v1/dns/zones/" + zid
	serial := func() float64 { return p.call(200, "GET", base, nil)["zone"].(map[string]any)["serial"].(float64) }
	s0 := serial()

	// A site with domains in the zone gets A and AAAA records for its server.
	site := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "blog", "domains": []string{"example.test", "www.example.test", "other.org"}, "tls": "internal",
	})["site"].(map[string]any)
	sid := jsonID(site["id"])
	eventually(t, "automatic records served", func() bool {
		_, a := dnsQuery(t, addr, "www.example.test.", mdns.TypeA)
		_, aaaa := dnsQuery(t, addr, "example.test.", mdns.TypeAAAA)
		return len(a) == 1 && strings.Contains(a[0], "300 IN A 203.0.113.10") && len(aaaa) == 1 && strings.Contains(aaaa[0], "2001:db8::10")
	})
	zone := p.call(200, "GET", base, nil)
	if len(zone["auto"].([]any)) != 4 || len(zone["records"].([]any)) != 0 {
		t.Fatalf("zone = %v", zone)
	}
	if a := zone["auto"].([]any)[0].(map[string]any); a["source"] != "site blog" {
		t.Fatalf("auto source = %v", a)
	}
	s1 := serial()
	if s1 <= s0 {
		t.Fatalf("serial did not change: %v -> %v", s0, s1)
	}

	// A record of the user's own wins over the automatic one.
	p.call(http.StatusCreated, "POST", base+"/records", map[string]any{"name": "www", "type": "CNAME", "value": "cdn.example.org"})
	eventually(t, "manual record wins", func() bool {
		_, a := dnsQuery(t, addr, "www.example.test.", mdns.TypeA)
		return len(a) == 1 && strings.Contains(a[0], "CNAME cdn.example.org.")
	})

	// New addresses for the server move the records.
	p.call(200, "PUT", srvPath+"/public-ips", map[string]any{"ips": []string{"203.0.113.20"}})
	eventually(t, "records follow the server", func() bool {
		_, a := dnsQuery(t, addr, "example.test.", mdns.TypeA)
		_, aaaa := dnsQuery(t, addr, "example.test.", mdns.TypeAAAA)
		return len(a) == 1 && strings.Contains(a[0], "203.0.113.20") && len(aaaa) == 0
	})

	// Domain changes and site removal remove them.
	p.call(200, "PUT", "/api/v1/sites/"+sid+"/domains", map[string]any{"domains": []string{"shop.example.test"}})
	eventually(t, "domain change", func() bool {
		rcode, _ := dnsQuery(t, addr, "example.test.", mdns.TypeA)
		_, shop := dnsQuery(t, addr, "shop.example.test.", mdns.TypeA)
		return rcode == mdns.RcodeSuccess && len(shop) == 1
	})
	if _, a := dnsQuery(t, addr, "example.test.", mdns.TypeA); len(a) != 0 {
		t.Fatalf("apex still has %v", a)
	}
	p.call(http.StatusOK, "DELETE", "/api/v1/sites/"+sid, nil)
	eventually(t, "site removed", func() bool {
		rcode, _ := dnsQuery(t, addr, "shop.example.test.", mdns.TypeA)
		return rcode == mdns.RcodeNameError
	})

	// Back to the detected addresses (none here: the agent connects over loopback).
	r = p.call(200, "PUT", srvPath+"/public-ips", map[string]any{"ips": []string{}})
	if ips := r["server"].(map[string]any)["public_ips"].([]any); len(ips) != 0 {
		t.Fatalf("public ips = %v", ips)
	}
}

func TestDNSSECEndToEnd(t *testing.T) {
	p := startPanel(t)
	startHostWith(t, p, "", func(c *agent.Config) { c.DNSListen = "127.0.0.1:0" })
	var addr string
	for _, s := range p.call(200, "GET", "/api/v1/dns", nil)["servers"].([]any) {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(s.(map[string]any)["port"].(float64))))
	}
	zid := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})["zone"].(map[string]any)["id"])
	base := "/api/v1/dns/zones/" + zid
	p.call(http.StatusCreated, "POST", base+"/records", map[string]any{"name": "@", "type": "A", "value": "192.0.2.1"})

	z := p.call(200, "PUT", base+"/dnssec", map[string]any{"enabled": true})["zone"].(map[string]any)
	sec := z["dnssec"].(map[string]any)
	ds := sec["ds"].([]any)[0].(string)
	if !strings.HasPrefix(ds, jsonID(sec["key_tag"])+" 13 2 ") {
		t.Fatalf("dnssec = %v", sec)
	}
	// Enabling again keeps the key (the DS at the registrar stays valid).
	if again := p.call(200, "PUT", base+"/dnssec", map[string]any{"enabled": true})["zone"].(map[string]any)["dnssec"].(map[string]any); again["ds"].([]any)[0] != ds {
		t.Fatal("key replaced")
	}
	// The private key is stored encrypted only.
	var sealed []byte
	p.db.R.QueryRow(`SELECT dnssec_key FROM dns_zones WHERE id = ?`, mustAtoi(zid)).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), "PrivateKey") {
		t.Fatal("private key not sealed")
	}

	ask := func(name string, qtype uint16) *mdns.Msg {
		q := new(mdns.Msg)
		q.SetQuestion(name, qtype)
		q.SetEdns0(1232, true)
		r, _, err := (&mdns.Client{Net: "tcp", Timeout: 2 * time.Second}).Exchange(q, addr)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var key *mdns.DNSKEY
	eventually(t, "zone signed", func() bool {
		r := ask("example.test.", mdns.TypeDNSKEY)
		if len(r.Answer) != 2 {
			return false
		}
		key, _ = r.Answer[0].(*mdns.DNSKEY)
		return key != nil
	})
	if d := key.ToDS(mdns.SHA256); !strings.HasSuffix(ds, strings.ToUpper(d.Digest)) {
		t.Fatal("served key does not match the DS shown")
	}
	r := ask("example.test.", mdns.TypeA)
	if len(r.Answer) != 2 {
		t.Fatalf("answer = %v", r.Answer)
	}
	if err := r.Answer[1].(*mdns.RRSIG).Verify(key, r.Answer[:1]); err != nil {
		t.Fatalf("signature: %v", err)
	}
	// A missing name: signed compact denial.
	r = ask("nope.example.test.", mdns.TypeA)
	var nsec *mdns.NSEC
	var nsecSig *mdns.RRSIG
	for _, rr := range r.Ns {
		switch v := rr.(type) {
		case *mdns.NSEC:
			nsec = v
		case *mdns.RRSIG:
			if v.TypeCovered == mdns.TypeNSEC {
				nsecSig = v
			}
		}
	}
	if r.Rcode != mdns.RcodeSuccess || nsec == nil || nsecSig == nil || nsecSig.Verify(key, []mdns.RR{nsec}) != nil {
		t.Fatalf("denial = %v", r)
	}

	// Turning it off removes the key and the signatures.
	z = p.call(200, "PUT", base+"/dnssec", map[string]any{"enabled": false})["zone"].(map[string]any)
	if z["dnssec"] != nil {
		t.Fatalf("zone = %v", z)
	}
	eventually(t, "signatures gone", func() bool { return len(ask("example.test.", mdns.TypeA).Answer) == 1 })
}

func TestDNSChallenges(t *testing.T) {
	p := startPanel(t)
	h := startHostWith(t, p, "", func(c *agent.Config) { c.DNSListen = "127.0.0.1:0" })
	other := startHost(t, p)
	var addr string
	for _, s := range p.call(200, "GET", "/api/v1/dns", nil)["servers"].([]any) {
		if s := s.(map[string]any); jsonID(s["server_id"]) == h.id {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(s["port"].(float64))))
		}
	}
	// A public wildcard needs the zone in Basalt (DNS-01); an internal one does not.
	route := func(want int, srv string, domains []string, tls string) {
		t.Helper()
		p.call(want, "POST", "/api/v1/servers/"+srv+"/routes", map[string]any{
			"kind": "reverse_proxy", "domains": domains, "upstream": "127.0.0.1:9", "tls": tls})
	}
	route(http.StatusUnprocessableEntity, h.id, []string{"*.example.com"}, "auto")
	route(http.StatusUnprocessableEntity, h.id, []string{"*.com"}, "auto")
	route(http.StatusCreated, h.id, []string{"*.internal.example.org"}, "internal")
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.com"})
	route(http.StatusCreated, h.id, []string{"*.example.com"}, "auto")
	route(http.StatusCreated, other.id, []string{"shop.example.com"}, "auto")

	// The proxy's DNS provider asks the agent, which asks the panel.
	prov := &caddyrun.DNSProvider{Socket: proxy.ACMESocket(h.proxyDir)}
	ctx := context.Background()
	txt := libdns.TXT{Name: "_acme-challenge", Text: "token-wild"}
	if _, err := prov.AppendRecords(ctx, "example.com.", []libdns.Record{txt}); err != nil {
		t.Fatal(err)
	}
	// Published on the DNS server by the time the call returns.
	q := func(name string) []string {
		_, ans := dnsQuery(t, addr, name, mdns.TypeTXT)
		return ans
	}
	if ans := q("_acme-challenge.example.com."); len(ans) != 1 || !strings.Contains(ans[0], `60 IN TXT "token-wild"`) {
		t.Fatalf("challenge = %v", ans)
	}
	// Shown with the zone's automatic records (the host's own public
	// addresses may be there too).
	shown := false
	for _, a := range p.call(200, "GET", "/api/v1/dns/zones/1", nil)["auto"].([]any) {
		a := a.(map[string]any)
		shown = shown || a["source"] == "ACME challenge" && a["value"] == "token-wild"
	}
	if !shown {
		t.Fatal("challenge not listed with the zone")
	}
	// Only for this server's own domains, and only in our zones.
	for zone, name := range map[string]string{
		"example.com.": "_acme-challenge.shop",            // another server's domain
		"example.net.": "_acme-challenge",                 // not served here
		"example.org.": "_acme-challenge.internal",        // served here, but not our zone
		"example.com":  "_acme-challenge.nope.deep.under", // nothing like it
	} {
		if _, err := prov.AppendRecords(ctx, zone, []libdns.Record{libdns.TXT{Name: name, Text: "x"}}); err == nil {
			t.Errorf("challenge %s in %s accepted", name, zone)
		}
	}
	if _, err := prov.DeleteRecords(ctx, "example.com.", []libdns.Record{txt}); err != nil {
		t.Fatal(err)
	}
	if ans := q("_acme-challenge.example.com."); len(ans) != 0 {
		t.Fatalf("challenge left: %v", ans)
	}
	// Expired challenges are dropped.
	if err := p.db.AddDNSChallenge(ctx, mustAtoi(h.id), "_acme-challenge.example.com", "old", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := p.db.DeleteExpiredDNSChallenges(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("expired: %d %v", n, err)
	}
}
