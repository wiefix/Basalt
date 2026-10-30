package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// Adminer in a PHP site: a page of the site, run by its PHP as its user,
// that reaches the server's MariaDB only. The agent keeps the truth (the
// page is a file of the site); the panel asks it.

type adminerView struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	URL       string `json:"url,omitempty"`
	Version   string `json:"version,omitempty"`
}

func (s *Server) adminerCall(ctx context.Context, serverID, siteID int64, op string) (agentproto.SiteAdminerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var resp agentproto.SiteAdminerResponse
	err := s.agents.Call(ctx, serverID, agentproto.TypeSiteAdminer, agentproto.SiteAdminer{SiteID: siteID, Op: op}, &resp)
	return resp, err
}

func viewAdminer(domains []string, resp agentproto.SiteAdminerResponse) adminerView {
	v := adminerView{Installed: resp.Path != "", Path: resp.Path, Version: resp.Version}
	if v.Installed && len(domains) > 0 && !strings.HasPrefix(domains[0], "*.") {
		v.URL = "https://" + domains[0] + resp.Path
	}
	return v
}

func (s *Server) handleSiteAdminer(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	op := agentproto.AdminerStatus
	switch r.Method {
	case http.MethodPost:
		op = agentproto.AdminerInstall
		if site.PHPVersion == "" {
			writeError(w, http.StatusUnprocessableEntity, "Adminer needs PHP: switch the site to a PHP version first")
			return
		}
		if v, _ := s.db.ServerMariaDB(r.Context(), site.ServerID); v == "" {
			writeError(w, http.StatusUnprocessableEntity, "Adminer needs MariaDB, which is not installed on this server")
			return
		}
	case http.MethodDelete:
		op = agentproto.AdminerRemove
	}
	if !s.agents.Connected(site.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	resp, err := s.adminerCall(r.Context(), site.ServerID, site.ID, op)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	switch op {
	case agentproto.AdminerInstall:
		s.audit(r, &p.user.ID, "site.adminer_installed", map[string]any{"site_id": site.ID, "version": resp.Version})
	case agentproto.AdminerRemove:
		s.audit(r, &p.user.ID, "site.adminer_removed", map[string]any{"site_id": site.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"adminer": viewAdminer(site.Domains, resp)})
}
