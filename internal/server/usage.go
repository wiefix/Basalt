package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

// Disk usage of sites: the agents measure each site's directory and its
// MariaDB databases every few hours (and on request); a site over its soft
// quota is reported once until it is back under.

const (
	usageEvery   = 6 * time.Hour
	usageTimeout = 30 * time.Minute
	maxQuotaMB   = 1 << 24 // 16 TiB
)

type siteUsageView struct {
	DiskBytes int64      `json:"disk_bytes"`
	DiskFiles int64      `json:"disk_files"`
	DBBytes   int64      `json:"db_bytes"`
	At        *time.Time `json:"at"`
}

func (s *Server) usageLoop(ctx context.Context) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(s.usageEvery)
		}
		servers, err := s.db.ListServers(ctx)
		if err != nil {
			s.log.Warn("list servers for disk usage", "err", err)
			continue
		}
		for _, sv := range servers {
			if !s.agents.Connected(sv.ID) {
				continue
			}
			sites, err := s.db.SitesOnServer(ctx, sv.ID)
			if err == nil && len(sites) > 0 {
				err = s.measureSites(ctx, sv.ID, sites)
			}
			if err != nil && ctx.Err() == nil {
				s.log.Warn("measure disk usage", "server_id", sv.ID, "err", err)
			}
		}
	}
}

// measureSites has the server's agent measure the sites and records the
// result, reporting sites that went over their quota.
func (s *Server) measureSites(ctx context.Context, serverID int64, sites []*store.Site) error {
	req := agentproto.SiteUsageRequest{Sites: []int64{}}
	dbSite := map[string]int64{}
	for _, site := range sites {
		req.Sites = append(req.Sites, site.ID)
		dbs, err := s.db.Databases(ctx, site.ID)
		if err != nil {
			return err
		}
		for _, d := range dbs {
			req.Databases = append(req.Databases, d.Name)
			dbSite[d.Name] = site.ID
		}
	}
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	var resp agentproto.SiteUsageResponse
	if err := s.agents.Stream(ctx, serverID, agentproto.TypeSiteUsage, req, &resp, func(json.RawMessage) error { return nil }); err != nil {
		return err
	}
	dbBytes := map[int64]int64{}
	for name, n := range resp.Databases {
		if id, ok := dbSite[name]; ok {
			dbBytes[id] += n
		}
	}
	now := s.now()
	byID := map[int64]*store.Site{}
	projects := map[int64]bool{}
	for _, site := range sites {
		byID[site.ID] = site
	}
	defer func() {
		for id := range projects {
			pr, err := s.db.Project(ctx, id)
			if err != nil {
				continue
			}
			if usage, err := s.db.ProjectUsage(ctx, id); err == nil {
				s.checkProjectDisk(ctx, pr, usage)
			}
		}
	}()
	for _, u := range resp.Sites {
		site := byID[u.SiteID]
		if site == nil || u.Error != "" {
			continue
		}
		site.Usage = store.SiteUsage{DiskBytes: max(0, u.Bytes), DiskFiles: max(0, u.Files), DBBytes: dbBytes[site.ID], At: &now}
		if err := s.db.SetSiteUsage(ctx, site.ID, site.Usage); err != nil {
			return err
		}
		s.checkQuota(ctx, site)
		projects[site.ProjectID] = true
	}
	return nil
}

// checkProjectDisk reports a project whose sites went over its disk limit,
// once (until they are back under).
func (s *Server) checkProjectDisk(ctx context.Context, pr *store.Project, usage store.ProjectUsage) {
	over := pr.Limits.DiskMB > 0 && usage.DiskBytes > int64(pr.Limits.DiskMB)<<20
	if over == pr.DiskAlerted {
		return
	}
	if err := s.db.SetProjectDiskAlerted(ctx, pr.ID, over); err != nil {
		s.log.Error("record project disk alert", "err", err)
		return
	}
	pr.DiskAlerted = over
	if over {
		s.notify(notify.Message{
			Event:   EventProjectDisk,
			Title:   fmt.Sprintf("Project %s is over its disk limit", pr.Name),
			Text:    fmt.Sprintf("Its sites use %s (files and databases) of %d MB.", humanSize(usage.DiskBytes), pr.Limits.DiskMB),
			Link:    fmt.Sprintf("/projects/%d", pr.ID),
			Project: pr.ID,
		})
	}
}

// checkQuota reports a site that went over its quota, once.
func (s *Server) checkQuota(ctx context.Context, site *store.Site) {
	if site.QuotaMB <= 0 {
		return
	}
	over := site.Usage.Total() > int64(site.QuotaMB)<<20
	if over == site.QuotaAlerted {
		return
	}
	if err := s.db.SetSiteQuotaAlerted(ctx, site.ID, over); err != nil {
		s.log.Error("record quota alert", "err", err)
		return
	}
	site.QuotaAlerted = over
	if over {
		s.notify(notify.Message{
			Event: EventSiteQuota,
			Title: fmt.Sprintf("Site %s is over its disk quota", site.Name),
			Text: fmt.Sprintf("It uses %s (files %s, databases %s) of %d MB.", humanSize(site.Usage.Total()),
				humanSize(site.Usage.DiskBytes), humanSize(site.Usage.DBBytes), site.QuotaMB),
			Link:    fmt.Sprintf("/sites/%d", site.ID),
			Project: site.ProjectID,
		})
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// handleMeasureSite measures a site's disk usage now.
func (s *Server) handleMeasureSite(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	if !s.agents.Connected(site.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	if err := s.measureSites(r.Context(), site.ServerID, []*store.Site{site}); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site)})
}

type siteQuotaRequest struct {
	QuotaMB int `json:"quota_mb"` // 0: none
}

// handleSetSiteQuota sets a site's soft disk quota.
func (s *Server) handleSetSiteQuota(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req siteQuotaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.QuotaMB < 0 || req.QuotaMB > maxQuotaMB {
		writeError(w, http.StatusUnprocessableEntity, "quota must be 0 (none) or a number of MB")
		return
	}
	if err := s.db.SetSiteQuota(r.Context(), site.ID, req.QuotaMB); err != nil {
		s.internalError(w, r, "set site quota", err)
		return
	}
	site.QuotaMB, site.QuotaAlerted = req.QuotaMB, false
	s.audit(r, &p.user.ID, "site.quota_changed", map[string]any{"site_id": site.ID, "quota_mb": req.QuotaMB})
	// Checked against the last measurement at once.
	if site.Usage.At != nil {
		s.checkQuota(r.Context(), site)
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site)})
}

// handleTrash lists the files of deleted sites waiting on a server (removed
// after 30 days); DELETE removes one at once.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request, p *principal) {
	s.trash(w, r, p, "")
}

func (s *Server) handleEmptyTrashEntry(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	if name == "" || len(name) > 64 {
		writeError(w, http.StatusNotFound, "no such file in the trash")
		return
	}
	s.trash(w, r, p, name)
}

func (s *Server) trash(w http.ResponseWriter, r *http.Request, p *principal, remove string) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	if !s.agents.Connected(srv.ID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	var resp agentproto.SiteTrashResponse
	if err := s.agents.Stream(ctx, srv.ID, agentproto.TypeSiteTrash, agentproto.SiteTrash{Remove: remove}, &resp, func(json.RawMessage) error { return nil }); err != nil {
		writeAgentError(w, err)
		return
	}
	if remove != "" {
		s.audit(r, &p.user.ID, "site.trash_removed", map[string]any{"server_id": srv.ID, "entry": remove})
	}
	if resp.Entries == nil {
		resp.Entries = []agentproto.TrashEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": resp.Entries, "keep_days": 30})
}

// handleSiteAccessLog and handleAppAccessLog return the latest requests
// (newest first) from the proxy's access log of a site or app.
func (s *Server) handleSiteAccessLog(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	s.accessLog(w, r, site.ServerID, agentproto.AccessLogRequest{SiteID: site.ID})
}

func (s *Server) handleAppAccessLog(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	s.accessLog(w, r, app.ServerID, agentproto.AccessLogRequest{AppID: app.ID})
}

func (s *Server) accessLog(w http.ResponseWriter, r *http.Request, serverID int64, req agentproto.AccessLogRequest) {
	req.Limit = 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		req.Limit = min(max(v, 1), 1000)
	}
	if !s.agents.Connected(serverID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var resp agentproto.AccessLogResponse
	if err := s.agents.Call(ctx, serverID, agentproto.TypeAccessLog, req, &resp); err != nil {
		writeAgentError(w, err)
		return
	}
	if resp.Entries == nil {
		resp.Entries = []agentproto.AccessEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": resp.Entries})
}

// handleRestoreTrash makes the files of a deleted site, in the trash of
// its server, a new site there: the site is recorded, the agent moves the
// folder in place and gives it to the new site's user, then the site is
// set up as any. Its databases and SFTP accounts went with the old site.
func (s *Server) handleRestoreTrash(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == "" || len(name) > 64 {
		writeError(w, http.StatusNotFound, "no such folder in the trash")
		return
	}
	var req createSiteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.agents.Connected(srv.ID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	req.ServerID = srv.ID
	site, ok := s.newSite(w, r, p, req)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	var list agentproto.SiteTrashResponse
	err := s.agents.Stream(ctx, srv.ID, agentproto.TypeSiteTrash, agentproto.SiteTrash{Restore: name, SiteID: site.ID}, &list,
		func(json.RawMessage) error { return nil })
	if err != nil {
		if _, derr := s.db.DeleteSite(context.WithoutCancel(r.Context()), site.ID); derr != nil {
			s.log.Error("remove the site of a failed restore", "site_id", site.ID, "err", derr)
		}
		writeAgentError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "site.restored", map[string]any{"site_id": site.ID, "server_id": srv.ID, "entry": name})
	resp := map[string]any{"site": viewSite(site)}
	if err := s.ensureSite(r.Context(), site); err != nil {
		resp["provision_error"] = err.Error()
	}
	s.PushProxy(r.Context(), site.ServerID)
	writeJSON(w, http.StatusCreated, resp)
}
