package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

const (
	// healthConfirm is how many evaluations in a row must show a new
	// status before it is taken (one when the status is unknown).
	healthConfirm = 2
	// healthFresh: a server's result counts for this many intervals.
	healthFresh = 3
	// healthEventsKept is how long status changes are kept.
	healthEventsKept = 90 * 24 * time.Hour
)

// healthVote is a server's latest result for a check.
type healthVote struct {
	OK        bool
	LatencyMS int
	Error     string
	At        time.Time
}

// healthState is what the panel knows of a check between status changes.
type healthState struct {
	check   store.HealthCheck
	votes   map[int64]healthVote // by server
	pending string               // the status the majority shows, not yet taken
	streak  int
}

// healthChecks holds the checks and their latest results in memory.
type healthChecks struct {
	mu     sync.Mutex
	states map[int64]*healthState
}

// loadHealth reads the checks (at start and after each change); results
// of unchanged checks are kept.
func (s *Server) loadHealth(ctx context.Context) error {
	checks, err := s.db.HealthChecks(ctx)
	if err != nil {
		return err
	}
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	states := map[int64]*healthState{}
	for _, c := range checks {
		st := &healthState{check: *c, votes: map[int64]healthVote{}}
		if old := s.health.states[c.ID]; old != nil && sameProbe(&old.check, c) {
			st.votes, st.pending, st.streak = old.votes, old.pending, old.streak
		}
		states[c.ID] = st
	}
	s.health.states = states
	return nil
}

func sameProbe(a, b *store.HealthCheck) bool {
	return a.Kind == b.Kind && a.Host == b.Host && a.Port == b.Port && a.Path == b.Path &&
		a.HostHeader == b.HostHeader && a.Insecure == b.Insecure && a.Status == b.Status
}

// HealthReport takes a server's results (hub callback) and re-evaluates
// the checks they are about.
func (s *Server) HealthReport(serverID int64, r agentproto.HealthReport) {
	now := s.now()
	type change struct {
		id             int64
		name, from     string
		status, detail string
	}
	var changes []change
	s.health.mu.Lock()
	seen := map[int64]bool{}
	for _, res := range r.Results {
		st := s.health.states[res.CheckID]
		if st == nil {
			continue
		}
		e := res.Error
		if len(e) > 200 {
			e = e[:200]
		}
		st.votes[serverID] = healthVote{OK: res.OK, LatencyMS: max(0, res.LatencyMS), Error: e, At: now}
		seen[res.CheckID] = true
	}
	for id := range seen {
		st := s.health.states[id]
		if status, detail, ok := evaluateHealth(st, now, s.agents.Connected); ok {
			changes = append(changes, change{id: id, name: st.check.Name, from: st.check.Status, status: status, detail: detail})
			st.check.Status = status
			st.check.StatusSince = &now
		}
	}
	s.health.mu.Unlock()
	pushDNS := false
	for _, c := range changes {
		dns, err := s.db.SetHealthStatus(context.Background(), c.id, c.status, c.detail, now)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("record health status", "check_id", c.id, "err", err)
			}
			continue
		}
		pushDNS = pushDNS || dns
		s.log.Info("health check changed", "check_id", c.id, "status", c.status, "detail", c.detail)
		switch {
		case c.status == store.HealthDown:
			s.notify(notify.Message{Event: EventHealthDown, Title: c.name + " is down", Text: c.detail, At: now, Link: "/monitoring"})
		case c.status == store.HealthUp && c.from == store.HealthDown:
			s.notify(notify.Message{Event: EventHealthUp, Title: c.name + " is back up", Text: c.detail, At: now, Link: "/monitoring"})
		}
	}
	// DNS records follow the check: the DNS servers get the zones without
	// (or again with) its addresses. Not on the agent's read loop.
	if pushDNS {
		go s.pushDNSAll(context.Background())
	}
}

// evaluateHealth decides a check's status from the fresh results of
// connected servers: the majority, confirmed by healthConfirm evaluations
// in a row. It reports a change. Callers hold s.health.mu.
func evaluateHealth(st *healthState, now time.Time, connected func(int64) bool) (string, string, bool) {
	fresh := time.Duration(healthFresh*st.check.Interval) * time.Second
	var up, down int
	var failures []string
	for id, v := range st.votes {
		if now.Sub(v.At) > fresh || !connected(id) {
			continue
		}
		if v.OK {
			up++
		} else {
			down++
			failures = append(failures, v.Error)
		}
	}
	total := up + down
	var majority string
	switch {
	case total == 0:
		return "", "", false
	case down*2 > total:
		majority = store.HealthDown
	case up*2 > total:
		majority = store.HealthUp
	default:
		return "", "", false // a tie keeps the status
	}
	if majority == st.check.Status {
		st.pending, st.streak = "", 0
		return "", "", false
	}
	if majority == st.pending {
		st.streak++
	} else {
		st.pending, st.streak = majority, 1
	}
	need := healthConfirm
	if st.check.Status == store.HealthUnknown {
		need = 1
	}
	if st.streak < need {
		return "", "", false
	}
	st.pending, st.streak = "", 0
	detail := fmt.Sprintf("%d of %d servers reach it", up, total)
	if majority == store.HealthDown {
		slices.Sort(failures)
		detail = fmt.Sprintf("%d of %d servers fail: %s", down, total, strings.Join(slices.Compact(failures), "; "))
	}
	return majority, detail, true
}

// healthApply is the set of checks agents run.
func (s *Server) healthApply(ctx context.Context) (agentproto.HealthApply, error) {
	rev, err := s.db.HealthRevision(ctx)
	if err != nil {
		return agentproto.HealthApply{}, err
	}
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	out := agentproto.HealthApply{Revision: rev, Checks: []agentproto.HealthCheck{}}
	for _, st := range s.health.states {
		out.Checks = append(out.Checks, probeOf(&st.check))
	}
	slices.SortFunc(out.Checks, func(a, b agentproto.HealthCheck) int { return int(a.ID - b.ID) })
	return out, nil
}

func probeOf(c *store.HealthCheck) agentproto.HealthCheck {
	return agentproto.HealthCheck{ID: c.ID, Kind: c.Kind, Host: c.Host, Port: c.Port, Path: c.Path,
		HostHeader: c.HostHeader, Insecure: c.Insecure, Interval: c.Interval, Timeout: c.Timeout}
}

// PushHealth sends the checks to an agent.
func (s *Server) PushHealth(ctx context.Context, serverID int64) {
	if !s.agents.Connected(serverID) {
		return
	}
	req, err := s.healthApply(ctx)
	if err != nil {
		s.log.Error("build health checks", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.agents.Call(ctx, serverID, agentproto.TypeHealthApply, req, nil); err != nil {
		s.log.Warn("health apply failed", "server_id", serverID, "err", err)
	}
}

func (s *Server) pushHealthAll(ctx context.Context) {
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		s.log.Error("list servers for health push", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Go(func() { s.PushHealth(ctx, srv.ID) })
	}
	wg.Wait()
}

// healthLoop forgets old status changes once a day.
func (s *Server) healthLoop(ctx context.Context) {
	if err := s.loadHealth(ctx); err != nil {
		s.log.Error("load health checks", "err", err)
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if err := s.db.DeleteHealthEventsBefore(ctx, s.now().Add(-healthEventsKept)); err != nil && ctx.Err() == nil {
			s.log.Warn("prune health events", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- API ---

type healthResultView struct {
	ServerID   int64     `json:"server_id"`
	ServerName string    `json:"server_name"`
	OK         bool      `json:"ok"`
	LatencyMS  int       `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
	Stale      bool      `json:"stale"`
}

type healthCheckView struct {
	ID          int64              `json:"id"`
	ProjectID   int64              `json:"project_id"`
	Name        string             `json:"name"`
	Kind        string             `json:"kind"`
	Host        string             `json:"host"`
	Port        int                `json:"port"`
	Path        string             `json:"path"`
	HostHeader  string             `json:"host_header"`
	Insecure    bool               `json:"insecure"`
	Interval    int                `json:"interval"`
	Timeout     int                `json:"timeout"`
	Status      string             `json:"status"`
	StatusSince *time.Time         `json:"status_since"`
	Results     []healthResultView `json:"results"`
}

func (s *Server) viewHealth(ctx context.Context) ([]healthCheckView, error) {
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, srv := range servers {
		names[srv.ID] = srv.Name
	}
	now := s.now()
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	out := []healthCheckView{}
	for _, st := range s.health.states {
		c := st.check
		v := healthCheckView{ID: c.ID, ProjectID: c.ProjectID, Name: c.Name, Kind: c.Kind, Host: c.Host, Port: c.Port, Path: c.Path,
			HostHeader: c.HostHeader, Insecure: c.Insecure, Interval: c.Interval, Timeout: c.Timeout, Status: c.Status,
			StatusSince: c.StatusSince, Results: []healthResultView{}}
		for id, vote := range st.votes {
			name, ok := names[id]
			if !ok {
				continue
			}
			v.Results = append(v.Results, healthResultView{ServerID: id, ServerName: name, OK: vote.OK, LatencyMS: vote.LatencyMS,
				Error: vote.Error, At: vote.At,
				Stale: now.Sub(vote.At) > time.Duration(healthFresh*c.Interval)*time.Second || !s.agents.Connected(id)})
		}
		slices.SortFunc(v.Results, func(a, b healthResultView) int { return strings.Compare(a.ServerName, b.ServerName) })
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b healthCheckView) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *Server) handleHealthChecks(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.viewHealth(r.Context())
	if err != nil {
		s.internalError(w, r, "list health checks", err)
		return
	}
	visible, err := s.visibleIn(r, p)
	if err != nil {
		s.internalError(w, r, "load project roles", err)
		return
	}
	out := []healthCheckView{}
	for _, c := range list {
		if visible(c.ProjectID) {
			// Which servers probe is the panel's business.
			if !p.user.Role.AtLeast(store.RoleViewer) {
				c.Results = []healthResultView{}
			}
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": out})
}

type healthCheckRequest struct {
	// ProjectID: the project of a new check (0: the default one); a change
	// keeps the project (move it with PUT …/project).
	ProjectID  int64  `json:"project_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Path       string `json:"path"`
	HostHeader string `json:"host_header"`
	Insecure   bool   `json:"insecure"`
	Interval   int    `json:"interval"`
	Timeout    int    `json:"timeout"`
}

// check validates a request into a check (defaults: port by kind, path
// /, every 30 s, 5 s timeout).
func (req *healthCheckRequest) check() (*store.HealthCheck, error) {
	c := &store.HealthCheck{ProjectID: req.ProjectID, Name: strings.TrimSpace(req.Name), Kind: req.Kind,
		Host: strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Host)), "."), Port: req.Port,
		Path: strings.TrimSpace(req.Path), HostHeader: strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.HostHeader)), "."),
		Insecure: req.Insecure, Interval: req.Interval, Timeout: req.Timeout}
	if !serverNameRE.MatchString(c.Name) {
		return nil, errors.New("name must be 1-63 characters: letters, digits, dot, dash, underscore")
	}
	if c.Port == 0 {
		c.Port = map[string]int{agentproto.HealthHTTP: 80, agentproto.HealthHTTPS: 443}[c.Kind]
	}
	if c.Kind != agentproto.HealthTCP && c.Path == "" {
		c.Path = "/"
	}
	if c.Interval == 0 {
		c.Interval = 30
	}
	if c.Timeout == 0 {
		c.Timeout = min(5, c.Interval-1)
	}
	p := probeOf(c)
	p.ID = 1
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Server) handleCreateHealthCheck(w http.ResponseWriter, r *http.Request, p *principal) {
	var req healthCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	project, ok := s.checkCreate(w, r, p, req.ProjectID)
	if !ok {
		return
	}
	req.ProjectID = project
	c, err := req.check()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if n, err := s.db.HealthChecks(r.Context()); err == nil && len(n) >= agentproto.MaxHealthChecks {
		writeError(w, http.StatusUnprocessableEntity, "too many health checks")
		return
	}
	if err := s.db.CreateHealthCheck(r.Context(), c); errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a health check with this name exists")
		return
	} else if projectError(w, err) {
		return
	} else if err != nil {
		s.internalError(w, r, "create health check", err)
		return
	}
	s.audit(r, &p.user.ID, "health.created", map[string]any{"check_id": c.ID, "name": c.Name, "target": healthTarget(c)})
	s.healthChanged(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{"check": map[string]any{"id": c.ID, "name": c.Name}})
}

func healthTarget(c *store.HealthCheck) string {
	t := c.Kind + "://" + c.Host + ":" + strconv.Itoa(c.Port) + c.Path
	if c.HostHeader != "" {
		t += " (Host: " + c.HostHeader + ")"
	}
	return t
}

// healthChanged reloads the checks and sends them to the agents.
func (s *Server) healthChanged(ctx context.Context) {
	if err := s.loadHealth(ctx); err != nil {
		s.log.Error("load health checks", "err", err)
	}
	go s.pushHealthAll(context.WithoutCancel(ctx))
}

func (s *Server) pathHealthCheck(w http.ResponseWriter, r *http.Request) (*store.HealthCheck, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		c, err := s.db.HealthCheck(r.Context(), id)
		if err == nil {
			if !s.checkProject(w, r, c.ProjectID, "health check not found") {
				return nil, false
			}
			return c, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load health check", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "health check not found")
	return nil, false
}

func (s *Server) handleUpdateHealthCheck(w http.ResponseWriter, r *http.Request, p *principal) {
	old, ok := s.pathHealthCheck(w, r)
	if !ok {
		return
	}
	var req healthCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := req.check()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.ProjectID != 0 && req.ProjectID != old.ProjectID {
		writeError(w, http.StatusUnprocessableEntity, "move a check to another project with PUT /api/v1/health-checks/{id}/project")
		return
	}
	c.ID, c.ProjectID = old.ID, old.ProjectID
	dns, err := s.db.UpdateHealthCheck(r.Context(), c)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a health check with this name exists")
		return
	} else if err != nil {
		s.internalError(w, r, "update health check", err)
		return
	}
	s.audit(r, &p.user.ID, "health.updated", map[string]any{"check_id": c.ID, "name": c.Name, "target": healthTarget(c)})
	s.healthChanged(r.Context())
	if dns {
		s.pushDNSAll(r.Context()) // the status starts over: every address is served again
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteHealthCheck(w http.ResponseWriter, r *http.Request, p *principal) {
	c, ok := s.pathHealthCheck(w, r)
	if !ok {
		return
	}
	dns, err := s.db.DeleteHealthCheck(r.Context(), c.ID)
	if err != nil {
		s.internalError(w, r, "delete health check", err)
		return
	}
	s.audit(r, &p.user.ID, "health.deleted", map[string]any{"check_id": c.ID, "name": c.Name})
	s.healthChanged(r.Context())
	if dns {
		s.pushDNSAll(r.Context())
	}
	w.WriteHeader(http.StatusNoContent)
}

// healthEventView is a status change of a health check.
type healthEventView struct {
	Status string    `json:"status"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

func (s *Server) handleHealthEvents(w http.ResponseWriter, r *http.Request, p *principal) {
	c, ok := s.pathHealthCheck(w, r)
	if !ok {
		return
	}
	events, err := s.db.HealthEvents(r.Context(), c.ID, 100)
	if err != nil {
		s.internalError(w, r, "list health events", err)
		return
	}
	out := make([]healthEventView, len(events))
	for i, e := range events {
		out[i] = healthEventView{e.Status, e.Detail, e.At}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
