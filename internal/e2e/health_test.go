package e2e

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
