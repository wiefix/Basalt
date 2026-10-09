package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

type routeView struct {
	ID        int64           `json:"id"`
	Kind      proxy.RouteKind `json:"kind"`
	Domains   []string        `json:"domains"`
	Root      string          `json:"root,omitempty"`
	Upstream  string          `json:"upstream,omitempty"`
	TLS       proxy.TLSMode   `json:"tls"`
	HSTS      bool            `json:"hsts"`
	CreatedAt time.Time       `json:"created_at"`
}

func viewRoute(r *store.ProxyRoute) routeView {
	return routeView{
		ID: r.ID, Kind: r.Kind, Domains: r.Domains, Root: r.Root, Upstream: r.Upstream,
		TLS: r.TLS, HSTS: r.HSTS, CreatedAt: r.CreatedAt,
	}
}

type proxySyncView struct {
	Revision        int64  `json:"revision"`
	AppliedRevision int64  `json:"applied_revision"`
	InSync          bool   `json:"in_sync"`
	Error           string `json:"error,omitempty"`
}

func (s *Server) proxySync(ctx context.Context, serverID int64) proxySyncView {
	st, err := s.db.ProxyStatus(ctx, serverID)
	if err != nil {
		return proxySyncView{Error: "status unavailable"}
	}
	return proxySyncView{
		Revision: st.Revision, AppliedRevision: st.AppliedRevision,
		InSync: st.AppliedRevision >= st.Revision, Error: st.Error,
	}
}

// PushProxy sends the server's full desired proxy state to its agent and
// records the outcome. Offline agents receive it when they reconnect.
func (s *Server) PushProxy(ctx context.Context, serverID int64) {
	if !s.agents.Connected(serverID) {
		return
	}
	d, err := s.db.DesiredProxy(ctx, serverID)
	if err != nil {
		s.log.Error("build desired proxy state", "err", err, "server_id", serverID)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var resp agentproto.ProxyApplyResponse
	applyErr := ""
	if err := s.agents.Call(ctx, serverID, agentproto.TypeProxyApply, d, &resp); err != nil {
		applyErr = err.Error()
		s.log.Warn("proxy apply failed", "server_id", serverID, "revision", d.Revision, "err", err)
	}
	if err := s.db.SetProxyStatus(context.WithoutCancel(ctx), serverID, d.Revision, applyErr); err != nil {
		s.log.Error("record proxy status", "err", err)
	}
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	routes, err := s.db.Routes(r.Context(), srv.ID)
	if err != nil {
		s.internalError(w, r, "list routes", err)
		return
	}
	out := make([]routeView, len(routes))
	for i, rt := range routes {
		out[i] = viewRoute(rt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": out, "sync": s.proxySync(r.Context(), srv.ID)})
}

type createRouteRequest struct {
	Kind     proxy.RouteKind `json:"kind"`
	Domains  []string        `json:"domains"`
	Root     string          `json:"root"`
	Upstream string          `json:"upstream"`
	TLS      proxy.TLSMode   `json:"tls"`
	HSTS     bool            `json:"hsts"`
}

func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	var req createRouteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	route := proxy.Route{
		Kind: req.Kind, Domains: req.Domains, Root: req.Root, Upstream: req.Upstream,
		TLS: req.TLS, HSTS: req.HSTS,
	}
	// The agent additionally confines static roots to its sites directory.
	if err := route.Validate(""); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	created, err := s.db.CreateRoute(r.Context(), srv.ID, route)
	var taken *store.ErrDomainTaken
	if errors.As(err, &taken) {
		writeError(w, http.StatusConflict, taken.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, "create route", err)
		return
	}
	s.audit(r, &p.user.ID, "route.created", map[string]any{
		"server_id": srv.ID, "route_id": created.ID, "kind": created.Kind, "domains": created.Domains,
	})
	s.PushProxy(r.Context(), srv.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"route": viewRoute(created), "sync": s.proxySync(r.Context(), srv.ID)})
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	routeID, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}
	if err := s.db.DeleteRoute(r.Context(), srv.ID, routeID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "route not found")
		return
	} else if err != nil {
		s.internalError(w, r, "delete route", err)
		return
	}
	s.audit(r, &p.user.ID, "route.deleted", map[string]any{"server_id": srv.ID, "route_id": routeID})
	s.PushProxy(r.Context(), srv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"sync": s.proxySync(r.Context(), srv.ID)})
}
