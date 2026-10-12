package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// healthReportEvery is how often results go to the panel.
const healthReportEvery = 2 * time.Second

// healthManager runs the panel's health checks from this host and reports
// their results; the panel decides from the results of all hosts.
type healthManager struct {
	log *slog.Logger

	mu       sync.Mutex
	revision int64
	running  map[int64]*healthRunner // by check id
	results  []agentproto.HealthResult
}

type healthRunner struct {
	check  agentproto.HealthCheck
	cancel context.CancelFunc
}

func newHealthManager(log *slog.Logger) *healthManager {
	return &healthManager{log: log, running: map[int64]*healthRunner{}}
}

// Apply replaces the checks; unchanged ones keep running on their schedule.
func (m *healthManager) Apply(ctx context.Context, req agentproto.HealthApply) error {
	if len(req.Checks) > agentproto.MaxHealthChecks {
		return errors.New("too many health checks")
	}
	for i := range req.Checks {
		if err := req.Checks[i].Validate(); err != nil {
			return fmt.Errorf("health check %d: %w", req.Checks[i].ID, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Revision < m.revision {
		return nil
	}
	m.revision = req.Revision
	want := map[int64]agentproto.HealthCheck{}
	for _, c := range req.Checks {
		want[c.ID] = c
	}
	for id, r := range m.running {
		if c, ok := want[id]; !ok || c != r.check {
			r.cancel()
			delete(m.running, id)
		}
	}
	for id, c := range want {
		if _, ok := m.running[id]; ok {
			continue
		}
		rctx, cancel := context.WithCancel(context.Background())
		m.running[id] = &healthRunner{check: c, cancel: cancel}
		go m.run(rctx, c)
	}
	return nil
}

// stop ends every check (the agent is shutting down).
func (m *healthManager) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.running {
		r.cancel()
		delete(m.running, id)
	}
}

func (m *healthManager) run(ctx context.Context, c agentproto.HealthCheck) {
	// Spread the first probes of many checks over a second.
	delay := time.Duration(c.ID%1000) * time.Millisecond
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r := probe(ctx, c)
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if len(m.results) < 10*agentproto.MaxHealthChecks {
			m.results = append(m.results, r)
		}
		m.mu.Unlock()
		t.Reset(time.Duration(c.Interval) * time.Second)
	}
}

// report sends results to the panel until the connection ends.
func (m *healthManager) report(ctx context.Context, peer *agentproto.Peer) {
	t := time.NewTicker(healthReportEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-peer.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		results := m.results
		m.results = nil
		m.mu.Unlock()
		if len(results) == 0 {
			continue
		}
		if err := peer.Notify(agentproto.TypeHealthReport, agentproto.HealthReport{Results: results}); err != nil {
			return
		}
	}
}

// probe runs one check.
func probe(ctx context.Context, c agentproto.HealthCheck) agentproto.HealthResult {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout)*time.Second)
	defer cancel()
	start := time.Now()
	err := probeErr(ctx, c)
	r := agentproto.HealthResult{CheckID: c.ID, OK: err == nil, LatencyMS: int(time.Since(start).Milliseconds())}
	if err != nil {
		r.Error = healthError(ctx, err)
	}
	return r
}

func probeErr(ctx context.Context, c agentproto.HealthCheck) error {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dialer := &net.Dialer{}
	if c.Kind == agentproto.HealthTCP {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	host := c.HostHeader
	if host == "" {
		host = c.Host
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:   &tls.Config{ServerName: host, InsecureSkipVerify: c.Insecure},
		DisableKeepAlives: true,
		Proxy:             nil,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// As browsers do, the default port is left out of the Host header.
	authority := net.JoinHostPort(host, strconv.Itoa(c.Port))
	if (c.Kind == agentproto.HealthHTTP && c.Port == 80) || (c.Kind == agentproto.HealthHTTPS && c.Port == 443) {
		authority = host
		if strings.Contains(host, ":") {
			authority = "[" + host + "]"
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Kind+"://"+authority+c.Path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Basalt-Health-Check")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// healthError shortens a probe error for the panel.
func healthError(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "no answer in time"
	}
	var ne *net.OpError
	if errors.As(err, &ne) && ne.Err != nil {
		err = ne.Err
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && strings.HasPrefix(s, "Get ") {
		s = s[i+2:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
