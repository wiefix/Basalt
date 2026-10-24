package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/standby"
	"github.com/wiefix/basalt/internal/store"
)

// Standby panels: copies of this panel that can take over (19.2). A
// standby gets the whole database and the master key, so only owners add
// and remove them.

// StandbyFeeder is what the panel knows of its standbys' streams.
type StandbyFeeder interface {
	State(id int64) standby.State
	Disconnect(id int64)
	// PushPanels sends the standbys the panel's addresses and their
	// failover setting again.
	PushPanels(ctx context.Context)
}

type standbyView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ServerID int64  `json:"server_id,omitempty"` // its host's agent
	// Enrolled: the standby has its certificate.
	Enrolled    bool       `json:"enrolled"`
	CertExpires *time.Time `json:"cert_expires_at"`
	LastSeen    *time.Time `json:"last_seen_at"`
	LastIP      string     `json:"last_ip"`
	Connected   bool       `json:"connected"`
	// Ready: its copy is complete; Applied: when it last took a change.
	Ready   bool       `json:"ready"`
	Applied *time.Time `json:"applied_at"`
	Error   string     `json:"error,omitempty"`
	Version string     `json:"version,omitempty"`
	// AgentURL is where agents reach it once it takes over.
	AgentURL string `json:"agent_url,omitempty"`
}

func (s *Server) viewStandby(sb *store.Standby) standbyView {
	v := standbyView{ID: sb.ID, Name: sb.Name, ServerID: sb.ServerID, Enrolled: sb.EnrolledAt != nil,
		CertExpires: sb.CertExpires, LastSeen: sb.LastSeen, LastIP: sb.LastIP, AgentURL: sb.AgentURL}
	if s.standbys == nil {
		return v
	}
	st := s.standbys.State(sb.ID)
	v.Connected, v.Ready, v.Error, v.Version = st.Connected, st.Report.Ready, st.Report.Error, st.Report.Version
	if !st.Report.Applied.IsZero() {
		at := st.Report.Applied
		v.Applied = &at
	}
	return v
}

type createStandbyRequest struct {
	Name string `json:"name"`
}

type standbyEnrollmentView struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// InstallCommand installs Basalt on a fresh host as a standby panel,
	// with an agent enrolled as server Name.
	InstallCommand string `json:"install_command"`
}

func (s *Server) handleListStandbys(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.Standbys(r.Context())
	if err != nil {
		s.internalError(w, r, "list standby panels", err)
		return
	}
	out := make([]standbyView, len(list))
	for i, sb := range list {
		out[i] = s.viewStandby(sb)
	}
	fo, err := s.viewFailover(r.Context())
	if err != nil {
		s.internalError(w, r, "failover setting", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"standbys": out, "available": s.standbys != nil, "failover": fo})
}

func (s *Server) handleCreateStandby(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.standbys == nil {
		writeError(w, http.StatusConflict, "this panel cannot feed standby panels")
		return
	}
	var req createStandbyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !serverNameRE.MatchString(name) {
		writeError(w, http.StatusUnprocessableEntity, "name must be 1-63 characters: letters, digits, dot, dash, underscore")
		return
	}
	// Its host runs an agent too: the standby takes over the panel, the
	// agent keeps serving that host's sites.
	agentToken, standbyToken := auth.NewToken(), auth.NewToken()
	expires := s.now().Add(enrollTokenTTL)
	srv, err := s.db.CreateServer(r.Context(), name, auth.HashToken(agentToken), expires)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a server with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create server", err)
		return
	}
	sb, err := s.db.CreateStandby(r.Context(), name, srv.ID, auth.HashToken(standbyToken), expires)
	if err != nil {
		s.db.DeleteServer(r.Context(), srv.ID)
		if errors.Is(err, store.ErrNameTaken) {
			writeError(w, http.StatusConflict, "a standby panel with this name already exists")
			return
		}
		s.internalError(w, r, "create standby panel", err)
		return
	}
	s.audit(r, &p.user.ID, "standby.created", map[string]any{"standby_id": sb.ID, "name": sb.Name, "server_id": srv.ID})
	agent := s.enrollment(agentToken, expires)
	cmd := strings.Replace(agent.InstallCommand, " | sh -s -- agent --panel ", " | sh -s -- standby --panel ", 1)
	cmd = strings.Replace(cmd, " --token "+agentToken, " --token "+standbyToken+" --agent-token "+agentToken, 1)
	writeJSON(w, http.StatusCreated, map[string]any{
		"standby":    s.viewStandby(sb),
		"server":     s.viewServer(srv),
		"enrollment": standbyEnrollmentView{Token: standbyToken, ExpiresAt: expires, InstallCommand: cmd},
	})
}

func (s *Server) handleDeleteStandby(w http.ResponseWriter, r *http.Request, p *principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "standby panel not found")
		return
	}
	sb, err := s.db.StandbyByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "standby panel not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load standby panel", err)
		return
	}
	if err := s.db.DeleteStandby(r.Context(), id); err != nil {
		s.internalError(w, r, "remove standby panel", err)
		return
	}
	if s.standbys != nil {
		s.standbys.Disconnect(id)
	}
	s.audit(r, &p.user.ID, "standby.removed", map[string]any{"standby_id": id, "name": sb.Name})
	// Agents forget its address.
	if h, ok := s.agents.(panelHub); ok {
		go h.PushPanelInfo(context.WithoutCancel(r.Context()))
	}
	w.WriteHeader(http.StatusNoContent)
}

// panelHub is the agent channel's view of the panel among its standbys
// (agenthub.Hub).
type panelHub interface {
	Superseded() (epoch int64, at time.Time, ok bool)
	PushPanelInfo(ctx context.Context)
}

type panelView struct {
	// Epoch counts the standbys that took over before this panel.
	Epoch int64 `json:"epoch"`
	// Superseded: a standby took over from this panel (its agents came with
	// a higher epoch and were refused); changes made here reach no server.
	Superseded *supersededView `json:"superseded"`
}

type supersededView struct {
	Epoch int64     `json:"epoch"`
	At    time.Time `json:"at"` // when an agent last told so
}

// handlePanel tells whether this panel is still the panel.
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request, p *principal) {
	epoch, err := s.db.PanelEpoch(r.Context())
	if err != nil {
		s.internalError(w, r, "load panel epoch", err)
		return
	}
	v := panelView{Epoch: epoch}
	if h, ok := s.agents.(panelHub); ok {
		if e, at, ok := h.Superseded(); ok {
			v.Superseded = &supersededView{Epoch: e, At: at}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"panel": v})
}
