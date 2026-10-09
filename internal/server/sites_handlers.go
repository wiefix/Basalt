package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

type siteView struct {
	ID        int64         `json:"id"`
	ServerID  int64         `json:"server_id"`
	Name      string        `json:"name"`
	Kind      string        `json:"kind"`
	Domains   []string      `json:"domains"`
	TLS       proxy.TLSMode `json:"tls"`
	CreatedAt time.Time     `json:"created_at"`
}

func viewSite(s *store.Site) siteView {
	return siteView{ID: s.ID, ServerID: s.ServerID, Name: s.Name, Kind: s.Kind,
		Domains: s.Domains, TLS: s.TLS, CreatedAt: s.CreatedAt}
}

func (s *Server) pathSite(w http.ResponseWriter, r *http.Request) (*store.Site, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "site not found")
		return nil, false
	}
	site, err := s.db.SiteByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "site not found")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "load site", err)
		return nil, false
	}
	return site, true
}

// normalizeDomains validates a domain list through the same rules the proxy uses.
func normalizeDomains(domains []string, tls proxy.TLSMode) ([]string, proxy.TLSMode, error) {
	r := proxy.Route{Kind: proxy.KindStatic, SiteID: 1, Domains: domains, TLS: tls}
	if err := r.Validate(""); err != nil {
		return nil, "", err
	}
	return r.Domains, r.TLS, nil
}

// SyncSites makes sure every site of the server exists on its agent, then
// pushes the proxy state. Called when an agent connects.
func (s *Server) SyncSites(ctx context.Context, serverID int64) {
	sites, err := s.db.SitesOnServer(ctx, serverID)
	if err != nil {
		s.log.Error("list sites for sync", "err", err, "server_id", serverID)
		return
	}
	for _, site := range sites {
		if err := s.ensureSite(ctx, site); err != nil {
			s.log.Warn("ensure site", "site_id", site.ID, "err", err)
		}
	}
	s.PushProxy(ctx, serverID)
	if err := s.PushSFTP(ctx, serverID); err != nil {
		s.log.Warn("push sftp accounts", "server_id", serverID, "err", err)
	}
}

func (s *Server) ensureSite(ctx context.Context, site *store.Site) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.agents.Call(ctx, site.ServerID, agentproto.TypeSiteEnsure, agentproto.SiteRequest{SiteID: site.ID}, nil)
}

type createSiteRequest struct {
	ServerID int64         `json:"server_id"`
	Name     string        `json:"name"`
	Domains  []string      `json:"domains"`
	TLS      proxy.TLSMode `json:"tls"`
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createSiteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !serverNameRE.MatchString(name) {
		writeError(w, http.StatusUnprocessableEntity, "name must be 1-63 characters: letters, digits, dot, dash, underscore")
		return
	}
	domains, tls, err := normalizeDomains(req.Domains, req.TLS)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, err := s.db.ServerByID(r.Context(), req.ServerID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "server not found")
		return
	} else if err != nil {
		s.internalError(w, r, "load server", err)
		return
	}

	site, err := s.db.CreateSite(r.Context(), req.ServerID, name, domains, tls)
	var taken *store.ErrDomainTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, taken.Error())
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "a site with this name already exists")
		return
	case err != nil:
		s.internalError(w, r, "create site", err)
		return
	}
	s.audit(r, &p.user.ID, "site.created", map[string]any{"site_id": site.ID, "name": site.Name, "domains": site.Domains})

	resp := map[string]any{"site": viewSite(site)}
	if s.agents.Connected(site.ServerID) {
		if err := s.ensureSite(r.Context(), site); err != nil {
			resp["provision_error"] = err.Error()
		}
		s.PushProxy(r.Context(), site.ServerID)
	}
	resp["sync"] = s.proxySync(r.Context(), site.ServerID)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request, p *principal) {
	sites, err := s.db.Sites(r.Context())
	if err != nil {
		s.internalError(w, r, "list sites", err)
		return
	}
	out := make([]siteView, len(sites))
	for i, site := range sites {
		out[i] = viewSite(site)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site), "sync": s.proxySync(r.Context(), site.ServerID)})
}

type siteDomainsRequest struct {
	Domains []string `json:"domains"`
}

func (s *Server) handleSetSiteDomains(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req siteDomainsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	domains, _, err := normalizeDomains(req.Domains, site.TLS)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	err = s.db.SetSiteDomains(r.Context(), site.ID, domains)
	var taken *store.ErrDomainTaken
	if errors.As(err, &taken) {
		writeError(w, http.StatusConflict, taken.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, "set site domains", err)
		return
	}
	s.audit(r, &p.user.ID, "site.domains_changed", map[string]any{"site_id": site.ID, "domains": domains})
	s.PushProxy(r.Context(), site.ServerID)
	site.Domains = domains
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site), "sync": s.proxySync(r.Context(), site.ServerID)})
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteSite(r.Context(), site.ID); err != nil {
		s.internalError(w, r, "delete site", err)
		return
	}
	s.audit(r, &p.user.ID, "site.deleted", map[string]any{"site_id": site.ID, "name": site.Name})
	// Stop serving and revoke SFTP access first, then move the files to the trash.
	s.PushProxy(r.Context(), site.ServerID)
	if err := s.db.BumpSFTPRevisionForServer(r.Context(), site.ServerID); err == nil {
		if err := s.PushSFTP(r.Context(), site.ServerID); err != nil {
			s.log.Warn("push sftp accounts", "err", err)
		}
	}
	resp := map[string]any{"sync": s.proxySync(r.Context(), site.ServerID)}
	if s.agents.Connected(site.ServerID) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := s.agents.Call(ctx, site.ServerID, agentproto.TypeSiteDelete, agentproto.SiteRequest{SiteID: site.ID}, nil); err != nil {
			resp["cleanup_error"] = err.Error()
		}
	} else {
		resp["cleanup_error"] = "server offline: files were left in place"
	}
	writeJSON(w, http.StatusOK, resp)
}
