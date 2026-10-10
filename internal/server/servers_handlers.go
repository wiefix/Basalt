package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/store"
)

// AgentHub is the control-plane side of the agent channel (agenthub.Hub).
type AgentHub interface {
	Connected(serverID int64) bool
	Call(ctx context.Context, serverID int64, msgType string, req, resp any) error
	Stream(ctx context.Context, serverID int64, msgType string, req, resp any, fn func(json.RawMessage) error) error
	Disconnect(serverID int64)
	CAFingerprint() string
}

const enrollTokenTTL = time.Hour

var serverNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

type serverView struct {
	ID             int64            `json:"id"`
	Name           string           `json:"name"`
	Status         string           `json:"status"` // pending, online, offline
	Hostname       string           `json:"hostname,omitempty"`
	OS             string           `json:"os,omitempty"`
	Arch           string           `json:"arch,omitempty"`
	Kernel         string           `json:"kernel,omitempty"`
	Distro         string           `json:"distro,omitempty"`
	AgentVersion   string           `json:"agent_version,omitempty"`
	CPUCount       int              `json:"cpu_count,omitempty"`
	MemTotal       uint64           `json:"mem_total,omitempty"`
	LastSeen       *time.Time       `json:"last_seen,omitempty"`
	LastIP         string           `json:"last_ip,omitempty"`
	CertExpires    *time.Time       `json:"cert_expires,omitempty"`
	EnrollExpires  *time.Time       `json:"enroll_expires,omitempty"`
	PHPVersions    []string         `json:"php_versions"`
	Isolated       bool             `json:"isolated"`
	Docker         string           `json:"docker"`
	DockerSecurity *docker.Security `json:"docker_security,omitempty"`
	Restic         string           `json:"restic"`
	CreatedAt      time.Time        `json:"created_at"`
}

func (s *Server) viewServer(srv *store.Server) serverView {
	status := "offline"
	switch {
	case !srv.Enrolled:
		status = "pending"
	case s.agents.Connected(srv.ID):
		status = "online"
	}
	return serverView{
		ID: srv.ID, Name: srv.Name, Status: status, Hostname: srv.Hostname, OS: srv.OS,
		Arch: srv.Arch, Kernel: srv.Kernel, Distro: srv.Distro, AgentVersion: srv.AgentVersion,
		CPUCount: srv.CPUCount, MemTotal: srv.MemTotal, LastSeen: srv.LastSeen, LastIP: srv.LastIP,
		CertExpires: srv.CertExpires, EnrollExpires: srv.EnrollExpires, CreatedAt: srv.CreatedAt,
		PHPVersions: srv.PHPVersions, Isolated: srv.Isolated, Docker: srv.Docker,
		DockerSecurity: srv.DockerSecurity, Restic: srv.Restic,
	}
}

type enrollmentView struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// InstallCommand installs and enrolls a fresh host in one go.
	InstallCommand string `json:"install_command"`
	// Command enrolls a host where basalt is already installed.
	Command string `json:"command"`
}

func (s *Server) enrollment(token string, expires time.Time) enrollmentView {
	fp := s.agents.CAFingerprint()
	hostport := strings.TrimPrefix(s.agentURL, "https://")
	args := "--panel " + s.agentURL + " --token " + token + " --ca-sha256 " + fp
	return enrollmentView{
		Token:     token,
		ExpiresAt: expires,
		// The CA is fetched without verification and then checked against the
		// fingerprint; everything after that is fetched trusting only that CA.
		InstallCommand: "curl -fsSk https://" + hostport + "/agent/v1/ca.der -o /tmp/basalt-ca.der" +
			` && echo "` + fp + `  /tmp/basalt-ca.der" | sha256sum -c --quiet -` +
			` && { echo '-----BEGIN CERTIFICATE-----'; base64 -w 64 /tmp/basalt-ca.der; echo '-----END CERTIFICATE-----'; } > /tmp/basalt-ca.pem` +
			" && curl -fsS --cacert /tmp/basalt-ca.pem --connect-to basalt-panel:443:" + hostport +
			" https://basalt-panel/agent/v1/install.sh | sh -s -- agent " + args,
		Command: "basalt agent enroll " + args,
	}
}

func (s *Server) pathServer(w http.ResponseWriter, r *http.Request) (*store.Server, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "server not found")
		return nil, false
	}
	srv, err := s.db.ServerByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "server not found")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "load server", err)
		return nil, false
	}
	return srv, true
}

type createServerRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createServerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !serverNameRE.MatchString(name) {
		writeError(w, http.StatusUnprocessableEntity,
			"name must be 1-63 characters: letters, digits, dot, dash, underscore")
		return
	}
	token := auth.NewToken()
	expires := s.now().Add(enrollTokenTTL)
	srv, err := s.db.CreateServer(r.Context(), name, auth.HashToken(token), expires)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a server with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create server", err)
		return
	}
	s.audit(r, &p.user.ID, "server.created", map[string]any{"server_id": srv.ID, "name": srv.Name})
	writeJSON(w, http.StatusCreated, map[string]any{
		"server":     s.viewServer(srv),
		"enrollment": s.enrollment(token, expires),
	})
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request, p *principal) {
	servers, err := s.db.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "list servers", err)
		return
	}
	out := make([]serverView, len(servers))
	for i, srv := range servers {
		out[i] = s.viewServer(srv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	resp := map[string]any{"server": s.viewServer(srv)}
	if m, err := s.db.MetricsSince(r.Context(), srv.ID, s.now().Add(-2*time.Minute), 1000); err == nil && len(m) > 0 {
		resp["metrics"] = viewMetrics(m[len(m)-1])
	}
	writeJSON(w, http.StatusOK, resp)
}

type metricsView struct {
	At        time.Time `json:"at"`
	CPUPct    float64   `json:"cpu_pct"`
	MemUsed   uint64    `json:"mem_used"`
	MemTotal  uint64    `json:"mem_total"`
	DiskUsed  uint64    `json:"disk_used"`
	DiskTotal uint64    `json:"disk_total"`
	Load1     float64   `json:"load1"`
	Load5     float64   `json:"load5"`
	Load15    float64   `json:"load15"`
	Uptime    uint64    `json:"uptime"`
}

func viewMetrics(m store.ServerMetrics) metricsView { return metricsView(m) }

func (s *Server) handleServerMetrics(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 48*60 {
			writeError(w, http.StatusUnprocessableEntity, "minutes must be between 1 and 2880")
			return
		}
		minutes = n
	}
	samples, err := s.db.MetricsSince(r.Context(), srv.ID, s.now().Add(-time.Duration(minutes)*time.Minute), 20000)
	if err != nil {
		s.internalError(w, r, "load metrics", err)
		return
	}
	out := make([]metricsView, len(samples))
	for i, m := range samples {
		out[i] = viewMetrics(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"metrics": out})
}

func (s *Server) handlePingServer(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	var resp agentproto.PingResponse
	if err := s.agents.Call(ctx, srv.ID, agentproto.TypePing, struct{}{}, &resp); err != nil {
		writeError(w, http.StatusBadGateway, "agent unreachable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rtt_ms":     float64(time.Since(start).Microseconds()) / 1000,
		"agent_time": resp.AgentTime,
	})
}

func (s *Server) handleResetEnrollToken(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	if srv.Enrolled {
		writeError(w, http.StatusConflict, "server is already enrolled; remove it and add it again to re-enroll")
		return
	}
	token := auth.NewToken()
	expires := s.now().Add(enrollTokenTTL)
	if err := s.db.ResetEnrollToken(r.Context(), srv.ID, auth.HashToken(token), expires); err != nil {
		s.internalError(w, r, "reset enroll token", err)
		return
	}
	s.audit(r, &p.user.ID, "server.enroll_token_reset", map[string]any{"server_id": srv.ID})
	writeJSON(w, http.StatusOK, map[string]any{"enrollment": s.enrollment(token, expires)})
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteServer(r.Context(), srv.ID); err != nil {
		s.internalError(w, r, "delete server", err)
		return
	}
	// The certificate fingerprint is gone with the row, so the agent cannot
	// reconnect; drop the live stream too.
	s.agents.Disconnect(srv.ID)
	s.audit(r, &p.user.ID, "server.deleted", map[string]any{"server_id": srv.ID, "name": srv.Name})
	w.WriteHeader(http.StatusNoContent)
}
