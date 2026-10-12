package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if r.Host != "app.example.com:"+r.URL.Query().Get("port") && r.Host != "app.example.com" {
				http.Error(w, "wrong host "+r.Host, http.StatusMisdirectedRequest)
				return
			}
			w.Write([]byte("fine"))
		case "/redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/slow":
			time.Sleep(2 * time.Second)
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tls.Close()
	host, ps, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(ps)
	_, tps, _ := net.SplitHostPort(strings.TrimPrefix(tls.URL, "https://"))
	tport, _ := strconv.Atoi(tps)
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	cport := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	check := func(c agentproto.HealthCheck) agentproto.HealthResult {
		t.Helper()
		c.ID, c.Interval = 1, 10
		if c.Timeout == 0 {
			c.Timeout = 5
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		return probe(context.Background(), c)
	}
	for name, tc := range map[string]struct {
		c   agentproto.HealthCheck
		ok  bool
		err string
	}{
		"ok with host":  {agentproto.HealthCheck{Kind: "http", Host: host, Port: port, Path: "/ok?port=" + ps, HostHeader: "app.example.com"}, true, ""},
		"wrong host":    {agentproto.HealthCheck{Kind: "http", Host: host, Port: port, Path: "/ok"}, false, "HTTP 421"},
		"redirect":      {agentproto.HealthCheck{Kind: "http", Host: host, Port: port, Path: "/redirect"}, true, ""},
		"down":          {agentproto.HealthCheck{Kind: "http", Host: host, Port: port, Path: "/x"}, false, "HTTP 503"},
		"slow":          {agentproto.HealthCheck{Kind: "http", Host: host, Port: port, Path: "/slow", Timeout: 1}, false, "no answer in time"},
		"refused":       {agentproto.HealthCheck{Kind: "http", Host: host, Port: cport, Path: "/"}, false, "refused"},
		"tcp":           {agentproto.HealthCheck{Kind: "tcp", Host: host, Port: port}, true, ""},
		"tcp refused":   {agentproto.HealthCheck{Kind: "tcp", Host: host, Port: cport}, false, "refused"},
		"tls untrusted": {agentproto.HealthCheck{Kind: "https", Host: host, Port: tport, Path: "/"}, false, "certificate"},
		"tls insecure":  {agentproto.HealthCheck{Kind: "https", Host: host, Port: tport, Path: "/", Insecure: true}, true, ""},
	} {
		r := check(tc.c)
		if r.OK != tc.ok || !strings.Contains(r.Error, tc.err) || r.CheckID != 1 {
			t.Errorf("%s: %+v", name, r)
		}
	}
	for _, bad := range []agentproto.HealthCheck{
		{ID: 1, Kind: "icmp", Host: host, Port: 1, Interval: 10, Timeout: 5},
		{ID: 1, Kind: "http", Host: "bad host", Port: 1, Path: "/", Interval: 10, Timeout: 5},
		{ID: 1, Kind: "http", Host: host, Port: 1, Path: "nope", Interval: 10, Timeout: 5},
		{ID: 1, Kind: "http", Host: host, Port: 1, Path: "/", Interval: 5, Timeout: 1},
		{ID: 1, Kind: "http", Host: host, Port: 1, Path: "/", Interval: 10, Timeout: 10},
		{ID: 1, Kind: "tcp", Host: host, Port: 1, Path: "/", Interval: 10, Timeout: 5},
		{ID: 0, Kind: "tcp", Host: host, Port: 1, Interval: 10, Timeout: 5},
		{ID: 1, Kind: "tcp", Host: host, Port: 70000, Interval: 10, Timeout: 5},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestHealthManager(t *testing.T) {
	m := newHealthManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.stop()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	c := agentproto.HealthCheck{ID: 7, Kind: "tcp", Host: "127.0.0.1", Port: port, Interval: 10, Timeout: 2}
	if err := m.Apply(context.Background(), agentproto.HealthApply{Revision: 2, Checks: []agentproto.HealthCheck{c}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.results)
		m.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no result")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !m.results[0].OK || m.results[0].CheckID != 7 {
		t.Fatalf("%+v", m.results)
	}
	// An older revision changes nothing; an empty newer one stops the check.
	m.Apply(context.Background(), agentproto.HealthApply{Revision: 1})
	if len(m.running) != 1 {
		t.Fatal("old revision applied")
	}
	m.Apply(context.Background(), agentproto.HealthApply{Revision: 3})
	if len(m.running) != 0 {
		t.Fatal("check kept")
	}
	bad := c
	bad.Port = 0
	if m.Apply(context.Background(), agentproto.HealthApply{Revision: 4, Checks: []agentproto.HealthCheck{bad}}) == nil {
		t.Fatal("invalid check accepted")
	}
}
