package e2e

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agent"
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
