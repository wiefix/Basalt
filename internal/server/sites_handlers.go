package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

type siteView struct {
	ID         int64         `json:"id"`
	ServerID   int64         `json:"server_id"`
	ProjectID  int64         `json:"project_id"`
	Name       string        `json:"name"`
	Kind       string        `json:"kind"`
	Domains    []string      `json:"domains"`
	TLS        proxy.TLSMode `json:"tls"`
	PHPVersion string        `json:"php_version"`
	PHP        php.Settings  `json:"php_settings"`
	Usage      siteUsageView `json:"usage"`
	QuotaMB    int           `json:"quota_mb"`
	OverQuota  bool          `json:"over_quota"`
	// Replicas are the other servers serving a copy of the files.
	Replicas  []int64   `json:"replicas"`
	CreatedAt time.Time `json:"created_at"`
}

func viewSite(s *store.Site) siteView {
	return siteView{ID: s.ID, ServerID: s.ServerID, ProjectID: s.ProjectID, Name: s.Name, Kind: s.Kind,
		Domains: s.Domains, TLS: s.TLS, PHPVersion: s.PHPVersion, PHP: s.PHP, CreatedAt: s.CreatedAt,
		Usage:   siteUsageView{DiskBytes: s.Usage.DiskBytes, DiskFiles: s.Usage.DiskFiles, DBBytes: s.Usage.DBBytes, At: s.Usage.At},
		QuotaMB: s.QuotaMB, OverQuota: s.QuotaMB > 0 && s.Usage.Total() > int64(s.QuotaMB)<<20, Replicas: s.Replicas}
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
	if !s.checkProject(w, r, site.ProjectID, "site not found") {
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
// pushes the proxy state, SFTP accounts, DNS zones and the list of apps. Called when
// an agent connects.
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
	// The sites it serves copies of.
	if replicas, err := s.db.SiteReplicasOn(ctx, serverID); err != nil {
		s.log.Error("list site replicas for sync", "err", err, "server_id", serverID)
	} else {
		for _, site := range replicas {
			if err := s.ensureSiteOn(ctx, site, serverID); err != nil {
				s.log.Warn("ensure site replica", "site_id", site.ID, "err", err)
			}
		}
	}
	s.PushProxy(ctx, serverID)
	s.PushPeers(ctx, serverID, true)
	s.pushSiteWatch(ctx, serverID)
	if err := s.PushSFTP(ctx, serverID); err != nil {
		s.log.Warn("push sftp accounts", "server_id", serverID, "err", err)
	}
	s.PushDNS(ctx, serverID)
	s.PushHealth(ctx, serverID)
	if err := s.SyncApps(ctx, serverID); err != nil {
		s.log.Warn("sync apps", "server_id", serverID, "err", err)
	}
}

// ensureSite makes sure the site's primary server has it.
func (s *Server) ensureSite(ctx context.Context, site *store.Site) error {
	return s.ensureSiteOn(ctx, site, site.ServerID)
}

type createSiteRequest struct {
	ProjectID  int64         `json:"project_id"` // 0: the default project
	ServerID   int64         `json:"server_id"`
	Name       string        `json:"name"`
	Domains    []string      `json:"domains"`
	TLS        proxy.TLSMode `json:"tls"`
	PHPVersion string        `json:"php_version"`
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createSiteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	site, ok := s.newSite(w, r, p, req)
	if !ok {
		return
	}
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

// newSite checks a new site and records it (on its server nothing is done
// yet); it answers the request itself when it cannot.
func (s *Server) newSite(w http.ResponseWriter, r *http.Request, p *principal, req createSiteRequest) (*store.Site, bool) {
	project, ok := s.checkCreate(w, r, p, req.ProjectID)
	if !ok {
		return nil, false
	}
	req.ProjectID = project
	name := strings.TrimSpace(req.Name)
	if !serverNameRE.MatchString(name) {
		writeError(w, http.StatusUnprocessableEntity, "name must be 1-63 characters: letters, digits, dot, dash, underscore")
		return nil, false
	}
	domains, tls, err := normalizeDomains(req.Domains, req.TLS)
	if err == nil {
		err = s.checkWildcards(r.Context(), domains, tls)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, false
	}
	if s.domainError(w, r, s.checkDomainProjects(r.Context(), domains, req.ProjectID)) {
		return nil, false
	}
	srv, err := s.db.ServerByID(r.Context(), req.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "server not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, r, "load server", err)
		return nil, false
	}
	if msg := checkPHP(srv, req.PHPVersion); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return nil, false
	}

	site, err := s.db.CreateSite(r.Context(), req.ProjectID, req.ServerID, name, domains, tls, req.PHPVersion)
	var taken *store.ErrDomainTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, taken.Error())
		return nil, false
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "a site with this name already exists")
		return nil, false
	case projectError(w, err):
		return nil, false
	case err != nil:
		s.internalError(w, r, "create site", err)
		return nil, false
	}
	s.audit(r, &p.user.ID, "site.created", map[string]any{"site_id": site.ID, "name": site.Name, "domains": site.Domains})
	return site, true
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request, p *principal) {
	sites, err := s.db.Sites(r.Context())
	if err != nil {
		s.internalError(w, r, "list sites", err)
		return
	}
	visible, err := s.visibleIn(r, p)
	if err != nil {
		s.internalError(w, r, "load project roles", err)
		return
	}
	out := []siteView{}
	for _, site := range sites {
		if visible(site.ProjectID) {
			out = append(out, viewSite(site))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	ps, err := s.db.SitePlacements(r.Context(), site.ID)
	if err != nil {
		s.internalError(w, r, "load site placements", err)
		return
	}
	pv, err := s.viewSitePlacements(r.Context(), site, ps)
	if err != nil {
		s.internalError(w, r, "load site placements", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site), "sync": s.proxySync(r.Context(), site.ServerID),
		"placements": pv})
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
	if err == nil {
		err = s.checkWildcards(r.Context(), domains, site.TLS)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if s.domainError(w, r, s.checkDomainProjects(r.Context(), domains, site.ProjectID)) {
		return
	}
	if len(site.Replicas) > 0 {
		// On several servers the domains stay in zones here.
		if msg, err := s.siteReplicaProblem(r.Context(), site, domains); err != nil {
			s.internalError(w, r, "check site", err)
			return
		} else if msg != "" {
			writeError(w, http.StatusUnprocessableEntity, msg)
			return
		}
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
	for _, sid := range site.Replicas {
		s.PushProxy(r.Context(), sid)
	}
	site.Domains = domains
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site), "sync": s.proxySync(r.Context(), site.ServerID)})
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	servers, err := s.db.DeleteSite(r.Context(), site.ID)
	if errors.Is(err, store.ErrSiteHasDatabases) {
		writeError(w, http.StatusConflict, "delete the site's databases first; they are not removed automatically to protect your data")
		return
	} else if err != nil {
		s.internalError(w, r, "delete site", err)
		return
	}
	s.audit(r, &p.user.ID, "site.deleted", map[string]any{"site_id": site.ID, "name": site.Name})
	if len(servers) > 1 {
		s.healthChanged(r.Context())
		s.syncAutoDNS(r.Context())
		s.startDBReplication()
		go s.pushSiteWatch(context.WithoutCancel(r.Context()), site.ServerID)
	}
	// Stop serving and revoke SFTP access first, then move the files to the trash.
	for _, sid := range servers {
		s.PushProxy(r.Context(), sid)
	}
	if err := s.db.BumpSFTPRevisionForServer(r.Context(), site.ServerID); err == nil {
		if err := s.PushSFTP(r.Context(), site.ServerID); err != nil {
			s.log.Warn("push sftp accounts", "err", err)
		}
	}
	resp := map[string]any{"sync": s.proxySync(r.Context(), site.ServerID)}
	var problems []string
	for _, sid := range servers {
		if err := s.deleteSiteOn(r.Context(), site.ID, sid); err != nil {
			if len(servers) > 1 {
				if srv, serr := s.db.ServerByID(r.Context(), sid); serr == nil {
					err = fmt.Errorf("%s: %w", srv.Name, err)
				}
			}
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		resp["cleanup_error"] = strings.Join(problems, "; ")
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkPHP validates a requested PHP version against what the server reports.
func checkPHP(srv *store.Server, version string) string {
	if version == "" {
		return ""
	}
	if !slices.Contains(srv.PHPVersions, version) {
		if len(srv.PHPVersions) == 0 {
			return "PHP is not installed on this server"
		}
		return "PHP " + version + " is not installed on this server (available: " + strings.Join(srv.PHPVersions, ", ") + ")"
	}
	if !srv.Isolated {
		return "PHP sites need per-site user isolation; run the agent as root on Linux"
	}
	return ""
}

type sitePHPRequest struct {
	Version string `json:"version"`
}

// handleSetSitePHP switches a site between static and a PHP version.
func (s *Server) handleSetSitePHP(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req sitePHPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	srv, err := s.db.ServerByID(r.Context(), site.ServerID)
	if err != nil {
		s.internalError(w, r, "load server", err)
		return
	}
	if msg := checkPHP(srv, req.Version); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	for _, sid := range site.Replicas {
		rs, err := s.db.ServerByID(r.Context(), sid)
		if err != nil {
			s.internalError(w, r, "load server", err)
			return
		}
		if msg := checkPHP(rs, req.Version); msg != "" {
			writeError(w, http.StatusUnprocessableEntity, rs.Name+" (serving a copy): "+msg)
			return
		}
	}
	if !s.agents.Connected(site.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline; try again when it reconnects")
		return
	}
	// Configure the pool first so the proxy never points at a missing socket.
	site.PHPVersion = req.Version
	if err := s.ensureSite(r.Context(), site); err != nil {
		writeAgentError(w, err)
		return
	}
	if err := s.ensureReplicas(r.Context(), site); err != nil {
		writeAgentError(w, err)
		return
	}
	if err := s.db.SetSitePHP(r.Context(), site.ID, req.Version); err != nil {
		s.internalError(w, r, "set site php", err)
		return
	}
	s.audit(r, &p.user.ID, "site.php_changed", map[string]any{"site_id": site.ID, "version": req.Version})
	s.PushProxy(r.Context(), site.ServerID)
	for _, sid := range site.Replicas {
		s.PushProxy(r.Context(), sid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site), "sync": s.proxySync(r.Context(), site.ServerID)})
}

// handleSetSitePHPSettings changes a site's PHP options (memory, upload
// size, time limits, error display, timezone).
func (s *Server) handleSetSitePHPSettings(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req php.Settings
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if site.PHPVersion != "" {
		if !s.agents.Connected(site.ServerID) {
			writeError(w, http.StatusConflict, "the server is offline; try again when it reconnects")
			return
		}
		// Applied first: a pool PHP-FPM rejects leaves the stored settings alone.
		next := *site
		next.PHP = req
		if err := s.ensureSite(r.Context(), &next); err != nil {
			writeAgentError(w, err)
			return
		}
		if err := s.ensureReplicas(r.Context(), &next); err != nil {
			writeAgentError(w, err)
			return
		}
	}
	if err := s.db.SetSitePHPSettings(r.Context(), site.ID, req); err != nil {
		s.internalError(w, r, "set site php settings", err)
		return
	}
	site.PHP = req
	s.audit(r, &p.user.ID, "site.php_settings_changed", map[string]any{"site_id": site.ID, "settings": req})
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site)})
}

const phpLogTail = 256 << 10

// handlePHPLog returns the end of the site's PHP error log, whole lines.
func (s *Server) handlePHPLog(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	const path = "/logs/php-error.log"
	st, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{Op: agentproto.FilesStat, Path: path}, fileOpTimeout)
	var re *agentproto.RemoteError
	if errors.As(err, &re) && (strings.Contains(re.Msg, "no such file") || strings.Contains(re.Msg, "does not exist")) {
		writeJSON(w, http.StatusOK, map[string]any{"log": "", "size": 0, "path": path})
		return
	}
	if err != nil {
		writeAgentError(w, err)
		return
	}
	size := st.Entry.Size
	off := max(0, size-phpLogTail)
	resp, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{Op: agentproto.FilesRead, Path: path, Offset: off, Length: phpLogTail}, fileOpTimeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	text := string(resp.Data)
	if off > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": strings.ToValidUTF8(text, "\uFFFD"), "size": size, "truncated": off > 0, "path": path})
}
