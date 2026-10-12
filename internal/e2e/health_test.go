package e2e

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/agentproto"
)

func TestHealthChecks(t *testing.T) {
	old := agentproto.MinHealthInterval
	agentproto.MinHealthInterval = 1
	t.Cleanup(func() { agentproto.MinHealthInterval = old })

	var status atomic.Int32
	status.Store(200)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, _, _ := net.SplitHostPort(r.Host); h != "app.example.com" {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(target.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	p := startPanel(t)
	startHost(t, p)
	startHost(t, p)
	create := func(want int, body map[string]any) map[string]any {
		return p.call(want, "POST", "/api/v1/health-checks", body)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "kind": "icmp", "host": "127.0.0.1"},
		{"name": "x", "kind": "http", "host": "not a host"},
		{"name": "x", "kind": "http", "host": "127.0.0.1", "path": "no-slash"},
		{"name": "x", "kind": "tcp", "host": "127.0.0.1"},
		{"name": "x", "kind": "http", "host": "127.0.0.1", "interval": 1000},
		{"name": "x", "kind": "http", "host": "127.0.0.1", "interval": 5, "timeout": 5},
		{"name": "bad name!", "kind": "http", "host": "127.0.0.1"},
	} {
		create(http.StatusUnprocessableEntity, bad)
	}
	web := create(http.StatusCreated, map[string]any{"name": "web", "kind": "http", "host": "127.0.0.1", "port": mustAtoi(port),
		"path": "/health", "host_header": "App.Example.com", "interval": 2, "timeout": 1})["check"].(map[string]any)
	create(http.StatusConflict, map[string]any{"name": "web", "kind": "tcp", "host": "127.0.0.1", "port": 1})
	create(http.StatusCreated, map[string]any{"name": "closed", "kind": "tcp", "host": "127.0.0.1", "port": closedPort,
		"interval": 2, "timeout": 1})
	wid := jsonID(web["id"])

	checkOf := func(name string) map[string]any {
		for _, c := range p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any) {
			if c := c.(map[string]any); c["name"] == name {
				return c
			}
		}
		return nil
	}
	wait := func(name, status string) map[string]any {
		t.Helper()
		var c map[string]any
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if c = checkOf(name); c["status"] == status {
				return c
			}
		}
		t.Fatalf("%s is not %s: %v", name, status, c)
		return nil
	}
	wait("web", "up")
	eventually(t, "results from both servers", func() bool { return len(checkOf("web")["results"].([]any)) == 2 })
	c := checkOf("web")
	if rs := c["results"].([]any); len(rs) != 2 || rs[0].(map[string]any)["ok"] != true || c["path"] != "/health" ||
		c["host_header"] != "app.example.com" {
		t.Fatalf("web = %v", c)
	}
	c = wait("closed", "down")
	if !strings.Contains(jsonString(c["results"]), "refused") {
		t.Fatalf("closed = %v", c)
	}

	// The target fails: both servers see it, and after confirmation it is down.
	status.Store(503)
	wait("web", "down")
	events := p.call(200, "GET", "/api/v1/health-checks/"+wid+"/events", nil)["events"].([]any)
	if len(events) != 2 || events[0].(map[string]any)["status"] != "down" ||
		!strings.Contains(events[0].(map[string]any)["detail"].(string), "2 of 2 servers fail: HTTP 503") ||
		events[1].(map[string]any)["status"] != "up" {
		t.Fatalf("events = %v", events)
	}
	status.Store(200)
	wait("web", "up")

	// A change starts the status over; deleting stops the probes.
	p.call(http.StatusNoContent, "PUT", "/api/v1/health-checks/"+wid, map[string]any{"name": "web2", "kind": "http",
		"host": "127.0.0.1", "port": mustAtoi(port), "path": "/other", "host_header": "app.example.com", "interval": 2, "timeout": 1})
	if c := checkOf("web2"); c == nil || c["status"] != "unknown" || len(c["results"].([]any)) != 0 {
		t.Fatalf("after update: %v", c)
	}
	wait("web2", "up")
	p.call(http.StatusNotFound, "PUT", "/api/v1/health-checks/999", map[string]any{"name": "x", "kind": "tcp", "host": "127.0.0.1", "port": 1})
	p.call(http.StatusNoContent, "DELETE", "/api/v1/health-checks/"+wid, nil)
	if checkOf("web2") != nil {
		t.Fatal("deleted check listed")
	}
	p.call(http.StatusNotFound, "GET", "/api/v1/health-checks/"+wid+"/events", nil)

	// Viewers see the checks but change nothing.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(200, "GET", "/api/v1/health-checks", nil)
	viewer.call(http.StatusForbidden, "POST", "/api/v1/health-checks", map[string]any{"name": "v", "kind": "tcp", "host": "127.0.0.1", "port": 1})
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(action) FROM audit_log WHERE action LIKE 'health.%'`).Scan(&audit)
	if audit != "health.created,health.created,health.updated,health.deleted" {
		t.Fatalf("audit: %s", audit)
	}
}

// TestDNSFailover: A records follow health checks; the DNS server stops
// answering an address once its check is down, and answers every address
// when all of them are down.
func TestDNSFailover(t *testing.T) {
	old := agentproto.MinHealthInterval
	agentproto.MinHealthInterval = 1
	t.Cleanup(func() { agentproto.MinHealthInterval = old })

	backend := func() (*atomic.Int32, int) {
		var status atomic.Int32
		status.Store(200)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(int(status.Load()))
		}))
		t.Cleanup(srv.Close)
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		return &status, int(mustAtoi(port))
	}
	statusA, portA := backend()
	statusB, portB := backend()

	p := startPanel(t)
	dnsHost := startHostWith(t, p, "", func(c *agent.Config) { c.DNSListen = "127.0.0.1:0" })
	startHost(t, p)
	var addr string
	for _, s := range p.call(200, "GET", "/api/v1/dns", nil)["servers"].([]any) {
		if s := s.(map[string]any); jsonID(s["server_id"]) == dnsHost.id {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(s["port"].(float64))))
		}
	}
	check := func(name string, port int) string {
		return jsonID(p.call(http.StatusCreated, "POST", "/api/v1/health-checks", map[string]any{"name": name, "kind": "http",
			"host": "127.0.0.1", "port": port, "interval": 2, "timeout": 1})["check"].(map[string]any)["id"])
	}
	checkA, checkB := check("a", portA), check("b", portB)

	zid := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})["zone"].(map[string]any)["id"])
	records := "/api/v1/dns/zones/" + zid + "/records"
	for _, bad := range []map[string]any{
		{"name": "www", "type": "A", "value": "192.0.2.1", "health_check": 999},
		{"name": "www", "type": "A", "value": "192.0.2.1", "ttl": 3600, "health_check": mustAtoi(checkA)},
		{"name": "alias", "type": "CNAME", "value": "example.net", "health_check": mustAtoi(checkA)},
	} {
		p.call(http.StatusUnprocessableEntity, "POST", records, bad)
	}
	rec := p.call(http.StatusCreated, "POST", records, map[string]any{"name": "www", "type": "A", "value": "192.0.2.1",
		"health_check": mustAtoi(checkA)})["record"].(map[string]any)
	if rec["ttl"].(float64) != 60 || jsonID(rec["health_check"]) != checkA {
		t.Fatalf("record = %v", rec)
	}
	p.call(http.StatusCreated, "POST", records, map[string]any{"name": "www", "type": "A", "value": "192.0.2.2",
		"health_check": mustAtoi(checkB)})

	served := func(want string) {
		t.Helper()
		var got []string
		for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			_, ans := dnsQuery(t, addr, "www.example.test.", mdns.TypeA)
			got = got[:0]
			for _, a := range ans {
				got = append(got, a[strings.LastIndex(a, " ")+1:])
			}
			slices.Sort(got)
			if strings.Join(got, " ") == want {
				return
			}
		}
		t.Fatalf("www answers %v, want %s", got, want)
	}
	served("192.0.2.1 192.0.2.2")

	// b fails: only a's address is served.
	statusB.Store(503)
	served("192.0.2.1")
	// a fails too: every address is served again.
	statusA.Store(503)
	waitStatus(t, p, "a", "down")
	served("192.0.2.1 192.0.2.2")
	// b recovers: only b.
	statusB.Store(200)
	served("192.0.2.2")

	// The zone shows which check each record follows; deleting the check
	// keeps the record, served unconditionally.
	zone := p.call(200, "GET", "/api/v1/dns/zones/"+zid, nil)
	if !strings.Contains(jsonString(zone["records"]), `"health_check":`+checkA) {
		t.Fatalf("zone records = %v", zone["records"])
	}
	p.call(http.StatusNoContent, "DELETE", "/api/v1/health-checks/"+checkA, nil)
	served("192.0.2.1 192.0.2.2")
	if strings.Contains(jsonString(p.call(200, "GET", "/api/v1/dns/zones/"+zid, nil)["records"]), `"health_check":`+checkA) {
		t.Fatal("deleted check still referenced")
	}
}

func waitStatus(t *testing.T, p *panel, name, status string) {
	t.Helper()
	var last any
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		for _, c := range p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any) {
			if c := c.(map[string]any); c["name"] == name {
				if c["status"] == status {
					return
				}
				last = c
			}
		}
	}
	t.Fatalf("%s is not %s: %v", name, status, last)
}
