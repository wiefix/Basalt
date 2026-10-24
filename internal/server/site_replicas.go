package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/store"
)

// A site can be served by several servers (replicas). Its files live on
// its primary server, where the file manager, SFTP, backups and PHP logs
// work; the panel copies them one way to the replicas every minute, so a
// change made on a replica itself is undone. DNS answers with the servers
// whose copy passes its health check, as for apps. A site's MariaDB
// databases are replicated to the copies separately (dbrepl.go), and the
// copies write to the primary's through the servers' tunnel.

const (
	siteSyncEvery   = time.Minute
	siteSyncTimeout = 30 * time.Minute
	// syncChunk is the most of one file moved in one read and write.
	syncChunk = 1 << 20
)

// sitePlacementView is a server serving a site, with the state of its copy
// of the files (replicas).
type sitePlacementView struct {
	ServerID    int64      `json:"server_id"`
	Primary     bool       `json:"primary"`
	HealthCheck int64      `json:"health_check,omitempty"`
	Status      string     `json:"status,omitempty"` // of the health check
	SyncedAt    *time.Time `json:"synced_at,omitempty"`
	SyncError   string     `json:"sync_error,omitempty"`
	Syncing     bool       `json:"syncing,omitempty"`
	// Databases is how the replica copies the site's databases (a site
	// without databases: none).
	Databases *dbCopyView `json:"databases,omitempty"`
}

// dbCopyView is how a replica copies a site's databases from its primary.
type dbCopyView struct {
	Running   bool      `json:"running"`
	Lag       int64     `json:"lag"` // seconds behind (-1: unknown)
	Error     string    `json:"error,omitempty"`
	Copied    []string  `json:"copied"` // of the site's databases
	Waiting   []string  `json:"waiting"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) viewSitePlacements(ctx context.Context, site *store.Site, ps []store.Placement) ([]sitePlacementView, error) {
	dbs, err := s.db.Databases(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	var repl []store.DBReplication
	if len(dbs) > 0 && len(ps) > 1 {
		if repl, err = s.db.DBReplications(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]sitePlacementView, len(ps))
	for i, p := range ps {
		out[i] = sitePlacementView{ServerID: p.ServerID, Primary: p.Primary, HealthCheck: p.HealthCheck, Status: p.CheckStatus,
			SyncedAt: p.SyncedAt, SyncError: p.SyncError}
		if p.Primary {
			continue
		}
		out[i].Syncing = s.siteSyncing(site.ID)
		if len(dbs) == 0 {
			continue
		}
		v := &dbCopyView{Lag: -1, Copied: []string{}, Waiting: []string{}}
		var row *store.DBReplication
		for j := range repl {
			if repl[j].ReplicaID == p.ServerID && repl[j].SourceID == site.ServerID {
				row = &repl[j]
			}
		}
		if row != nil {
			v.Running, v.Lag, v.Error, v.UpdatedAt = row.Running, row.Lag, row.Error, row.UpdatedAt
		}
		for _, d := range dbs {
			if row != nil && slices.Contains(row.Databases, d.Name) {
				v.Copied = append(v.Copied, d.Name)
			} else {
				v.Waiting = append(v.Waiting, d.Name)
			}
		}
		out[i].Databases = v
	}
	return out, nil
}

// siteReplicaProblem says why a site cannot be served by several servers
// ("" if it can).
func (s *Server) siteReplicaProblem(ctx context.Context, site *store.Site, domains []string) (string, error) {
	if len(domains) == 0 {
		return "add the site's domains first: traffic reaches the servers through them", nil
	}
	return s.domainsOutsideZones(ctx, domains)
}

func (s *Server) handleAddSiteReplica(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req replicaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg, err := s.siteReplicaProblem(r.Context(), site, site.Domains); err != nil {
		s.internalError(w, r, "check site", err)
		return
	} else if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if req.ServerID == site.ServerID || slices.Contains(site.Replicas, req.ServerID) {
		writeError(w, http.StatusConflict, "the server already serves the site")
		return
	}
	srv, err := s.db.ServerByID(r.Context(), req.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "server not found")
		return
	} else if err != nil {
		s.internalError(w, r, "load server", err)
		return
	}
	if msg := checkPHP(srv, site.PHPVersion); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, srv.Name+": "+msg)
		return
	}
	// The site's databases are copied there from its server.
	if dbs, err := s.db.Databases(r.Context(), site.ID); err != nil {
		s.internalError(w, r, "list databases", err)
		return
	} else if len(dbs) > 0 {
		primary, err := s.db.ServerByID(r.Context(), site.ServerID)
		if err != nil {
			s.internalError(w, r, "load server", err)
			return
		}
		if msg := replicaDBProblem(primary, srv); msg != "" {
			writeError(w, http.StatusUnprocessableEntity, msg)
			return
		}
	}
	if !s.agents.Connected(srv.ID) {
		writeError(w, http.StatusConflict, srv.Name+" is offline; try again when it reconnects")
		return
	}
	check, err := placementCheck("site", site.ID, site.Name, site.Domains, srv)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var primaryCheck *store.HealthCheck
	if len(site.Replicas) == 0 {
		primary, err := s.db.ServerByID(r.Context(), site.ServerID)
		if err != nil {
			s.internalError(w, r, "load server", err)
			return
		}
		if primaryCheck, err = placementCheck("site", site.ID, site.Name, site.Domains, primary); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	// The site's user, folders and PHP pool there first, so the proxy
	// never serves a missing site.
	if err := s.ensureSiteOn(r.Context(), site, srv.ID); err != nil {
		writeAgentError(w, err)
		return
	}
	err = s.db.AddSiteReplica(r.Context(), site.ID, srv.ID, check, primaryCheck)
	switch {
	case errors.Is(err, store.ErrReplicaExists):
		writeError(w, http.StatusConflict, "the server already serves the site")
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "a health check named "+check.Name+" exists: rename it under Monitoring")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "site not found")
		return
	case err != nil && strings.Contains(err.Error(), "already served"):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "add site replica", err)
		return
	}
	s.audit(r, &p.user.ID, "site.replica_added", map[string]any{"site_id": site.ID, "server_id": srv.ID, "check_id": check.ID})
	s.healthChanged(r.Context())
	// Every route of the site now takes its certificates through DNS-01.
	s.PushProxy(r.Context(), site.ServerID)
	s.PushProxy(r.Context(), srv.ID)
	s.syncAutoDNS(r.Context())
	s.startSiteSync(site.ID)
	s.startDBReplication()
	s.writeSite(w, r, http.StatusCreated, site.ID)
}

func (s *Server) handleRemoveSiteReplica(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil || !slices.Contains(site.Replicas, sid) {
		writeError(w, http.StatusNotFound, "the server does not serve a copy of the site")
		return
	}
	if err := s.db.RemoveSiteReplica(r.Context(), site.ID, sid); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the server does not serve a copy of the site")
		return
	} else if err != nil {
		s.internalError(w, r, "remove site replica", err)
		return
	}
	s.audit(r, &p.user.ID, "site.replica_removed", map[string]any{"site_id": site.ID, "server_id": sid})
	s.healthChanged(r.Context())
	s.syncAutoDNS(r.Context())
	s.PushProxy(r.Context(), sid)
	s.PushProxy(r.Context(), site.ServerID)
	s.startDBReplication()
	var cleanup string
	if err := s.deleteSiteOn(r.Context(), site.ID, sid); err != nil {
		cleanup = err.Error()
	}
	s.writeSite(w, r, http.StatusOK, site.ID, "cleanup_error", cleanup)
}

// handleSyncSite copies the site's files to its replicas now (in the
// background; the placements show how it went).
func (s *Server) handleSyncSite(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	if len(site.Replicas) == 0 {
		writeError(w, http.StatusConflict, "the site is served by one server: there is nothing to copy")
		return
	}
	s.startSiteSync(site.ID)
	s.startDBReplication()
	s.writeSite(w, r, http.StatusAccepted, site.ID)
}

// writeSite answers with a site and where it is served, plus extra keys
// whose value is not empty.
func (s *Server) writeSite(w http.ResponseWriter, r *http.Request, status int, id int64, extra ...string) {
	site, err := s.db.SiteByID(r.Context(), id)
	if err != nil {
		s.internalError(w, r, "load site", err)
		return
	}
	ps, err := s.db.SitePlacements(r.Context(), id)
	if err != nil {
		s.internalError(w, r, "load site placements", err)
		return
	}
	pv, err := s.viewSitePlacements(r.Context(), site, ps)
	if err != nil {
		s.internalError(w, r, "load site placements", err)
		return
	}
	resp := map[string]any{"site": viewSite(site), "placements": pv}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] != "" {
			resp[extra[i]] = extra[i+1]
		}
	}
	writeJSON(w, status, resp)
}

// ensureSiteOn makes sure a server has the site (its user, folders and PHP
// pool).
func (s *Server) ensureSiteOn(ctx context.Context, site *store.Site, serverID int64) error {
	req := agentproto.SiteRequest{SiteID: site.ID, PHPVersion: site.PHPVersion, PHP: site.PHP}
	if serverID != site.ServerID {
		// A copy reaches the databases on the site's server, if it has any.
		dbs, err := s.db.Databases(ctx, site.ID)
		if err != nil {
			return err
		}
		primary, err := s.db.ServerByID(ctx, site.ServerID)
		if err != nil {
			return err
		}
		if addr := peerAddr(primary); addr != "" && len(dbs) > 0 {
			req.DB = &agentproto.SiteDBRoute{ServerID: primary.ID, Addr: addr}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.agents.Call(ctx, serverID, agentproto.TypeSiteEnsure, req, nil)
}

// ensureReplicas applies a site's PHP setup on its connected replicas (the
// others get it when they connect); it returns the first failure.
func (s *Server) ensureReplicas(ctx context.Context, site *store.Site) error {
	var first error
	for _, sid := range site.Replicas {
		if !s.agents.Connected(sid) {
			continue
		}
		if err := s.ensureSiteOn(ctx, site, sid); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// deleteSiteOn moves a site's files on a server to its trash.
func (s *Server) deleteSiteOn(ctx context.Context, siteID, serverID int64) error {
	if !s.agents.Connected(serverID) {
		return errors.New("server offline: files were left in place")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.agents.Call(ctx, serverID, agentproto.TypeSiteDelete, agentproto.SiteRequest{SiteID: siteID}, nil)
}

// --- copying files ---

// siteSyncs tracks the sites being copied (one copy of a site at a time).
type siteSyncs struct {
	mu      sync.Mutex
	running map[int64]bool
}

func (s *Server) siteSyncing(id int64) bool {
	s.syncs.mu.Lock()
	defer s.syncs.mu.Unlock()
	return s.syncs.running[id]
}

// startSiteSync copies a site's files to its replicas in the background,
// unless that is going on already.
func (s *Server) startSiteSync(id int64) {
	s.syncs.mu.Lock()
	if s.syncs.running == nil {
		s.syncs.running = map[int64]bool{}
	}
	if s.syncs.running[id] {
		s.syncs.mu.Unlock()
		return
	}
	s.syncs.running[id] = true
	s.syncs.mu.Unlock()
	go func() {
		defer func() {
			s.syncs.mu.Lock()
			delete(s.syncs.running, id)
			s.syncs.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), siteSyncTimeout)
		defer cancel()
		s.syncSite(ctx, id)
	}()
}

// siteSyncLoop copies the files of every site on several servers, every
// minute.
func (s *Server) siteSyncLoop(ctx context.Context) {
	t := time.NewTicker(s.siteSyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sites, err := s.db.ReplicatedSites(ctx)
		if err != nil {
			s.log.Warn("list replicated sites", "err", err)
			continue
		}
		for _, site := range sites {
			s.startSiteSync(site.ID)
		}
		go s.syncDBReplication(ctx)
	}
}

// syncSite copies a site's files to each of its connected replicas and
// records how it went.
func (s *Server) syncSite(ctx context.Context, id int64) {
	site, err := s.db.SiteByID(ctx, id)
	if err != nil {
		return
	}
	for _, sid := range site.Replicas {
		if !s.agents.Connected(site.ServerID) {
			return // tried again when the primary is back
		}
		if !s.agents.Connected(sid) {
			continue
		}
		start := s.now()
		// A file that changed while being copied goes next time.
		_, err := s.copySite(ctx, site.ID, site.ServerID, sid)
		msg := ""
		if err != nil {
			msg = err.Error()
			s.log.Warn("copy site files", "site_id", site.ID, "server_id", sid, "err", err)
		}
		if err := s.db.SetSiteSync(ctx, site.ID, sid, start, msg); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("record site sync", "err", err)
		}
	}
}

type syncStats struct {
	deleted, dirs, files, chmods int
	bytes                        int64
	// changed counts files that changed while being copied (left for the
	// next round).
	changed int
}

// manifest fetches the list of a site's files on a server.
func (s *Server) manifest(ctx context.Context, siteID, serverID int64) (map[string]agentproto.ManifestEntry, []agentproto.ManifestEntry, error) {
	var list []agentproto.ManifestEntry
	var resp agentproto.SiteManifestResponse
	err := s.agents.Stream(ctx, serverID, agentproto.TypeSiteManifest, agentproto.SiteManifestRequest{SiteID: siteID}, &resp,
		func(raw json.RawMessage) error {
			var f agentproto.ManifestFrame
			if err := json.Unmarshal(raw, &f); err != nil {
				return err
			}
			if len(list)+len(f.Entries) > agentproto.MaxManifest {
				return errors.New("too many files")
			}
			list = append(list, f.Entries...)
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	byPath := make(map[string]agentproto.ManifestEntry, len(list))
	for _, e := range list {
		byPath[e.Path] = e
	}
	return byPath, list, nil
}

// copySite makes the files of a site on dst what they are on src: what
// dst has and src does not goes, folders are made, files that differ in
// size or time are copied whole, and modes follow.
func (s *Server) copySite(ctx context.Context, siteID, src, dst int64) (syncStats, error) {
	var st syncStats
	from, fromList, err := s.manifest(ctx, siteID, src)
	if err != nil {
		return st, fmt.Errorf("list the files on the primary: %w", err)
	}
	to, toList, err := s.manifest(ctx, siteID, dst)
	if err != nil {
		return st, fmt.Errorf("list the files on the replica: %w", err)
	}

	// Deletions: what the primary does not have (or has as the other
	// kind), the topmost path only.
	var del []string
	sort.Slice(toList, func(i, j int) bool { return toList[i].Path < toList[j].Path })
	gone := ""
	for _, e := range toList {
		if gone != "" && strings.HasPrefix(e.Path, gone+"/") {
			continue
		}
		if f, ok := from[e.Path]; !ok || f.Dir != e.Dir {
			del = append(del, e.Path)
			delete(to, e.Path)
			if e.Dir {
				gone = e.Path
				for p := range to {
					if strings.HasPrefix(p, e.Path+"/") {
						delete(to, p)
					}
				}
			}
		}
	}
	// Folders and modes; then the files that differ.
	var mkdir, chmod []agentproto.ManifestEntry
	var files []agentproto.ManifestEntry
	for _, e := range fromList {
		t, ok := to[e.Path]
		switch {
		case e.Dir && !ok:
			mkdir = append(mkdir, e)
		case e.Dir && t.Mode != e.Mode:
			chmod = append(chmod, e)
		case e.Dir:
		case !ok || t.Size != e.Size || t.MTime != e.MTime:
			files = append(files, e)
		case t.Mode != e.Mode:
			chmod = append(chmod, e)
		}
	}
	write := func(req agentproto.SiteSyncWriteRequest) error {
		req.SiteID = siteID
		c, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return s.agents.Call(c, dst, agentproto.TypeSiteSyncWrite, req, nil)
	}
	for len(del) > 0 {
		n := min(len(del), agentproto.MaxSyncOps)
		if err := write(agentproto.SiteSyncWriteRequest{Delete: del[:n]}); err != nil {
			return st, err
		}
		st.deleted += n
		del = del[n:]
	}
	for len(mkdir) > 0 {
		n := min(len(mkdir), agentproto.MaxSyncOps)
		if err := write(agentproto.SiteSyncWriteRequest{Mkdir: mkdir[:n]}); err != nil {
			return st, err
		}
		st.dirs += n
		mkdir = mkdir[n:]
	}

	// Files, in batches of reads up to MaxSyncData, each file in chunks of
	// syncChunk; a file is put in place with its last chunk.
	type piece struct {
		e   agentproto.ManifestEntry
		off int64
		n   int
	}
	var batch []piece
	size := 0
	skip := map[string]bool{} // changed since the manifest
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		reads := make([]agentproto.SyncRead, len(batch))
		for i, p := range batch {
			reads[i] = agentproto.SyncRead{Path: p.e.Path, Offset: p.off, Length: p.n}
		}
		c, cancel := context.WithTimeout(ctx, 5*time.Minute)
		var got agentproto.SiteSyncReadResponse
		err := s.agents.Call(c, src, agentproto.TypeSiteSyncRead, agentproto.SiteSyncReadRequest{SiteID: siteID, Reads: reads}, &got)
		cancel()
		if err != nil {
			return err
		}
		if len(got.Chunks) != len(batch) {
			return errors.New("the primary answered a different read")
		}
		var w agentproto.SiteSyncWriteRequest
		for i, p := range batch {
			c := got.Chunks[i]
			if skip[p.e.Path] {
				continue
			}
			if c.Missing || c.Size != p.e.Size || c.MTime != p.e.MTime || len(c.Data) != p.n {
				skip[p.e.Path] = true
				st.changed++
				continue
			}
			final := p.off+int64(p.n) >= p.e.Size
			w.Write = append(w.Write, agentproto.SyncWrite{Path: p.e.Path, Offset: p.off, Data: c.Data, Final: final, Mode: p.e.Mode, MTime: p.e.MTime})
			st.bytes += int64(p.n)
			if final {
				st.files++
			}
		}
		batch, size = batch[:0], 0
		if len(w.Write) == 0 {
			return nil
		}
		return write(w)
	}
	for _, e := range files {
		for off := int64(0); ; {
			n := int(min(e.Size-off, syncChunk))
			if size+n > agentproto.MaxSyncData || len(batch) == agentproto.MaxSyncOps {
				if err := flush(); err != nil {
					return st, err
				}
			}
			batch = append(batch, piece{e, off, n})
			size += n
			off += int64(n)
			if off >= e.Size {
				break
			}
		}
	}
	if err := flush(); err != nil {
		return st, err
	}
	for len(chmod) > 0 {
		n := min(len(chmod), agentproto.MaxSyncOps)
		if err := write(agentproto.SiteSyncWriteRequest{Chmod: chmod[:n]}); err != nil {
			return st, err
		}
		st.chmods += n
		chmod = chmod[n:]
	}
	return st, nil
}

// --- switching the primary ---

type sitePrimaryRequest struct {
	ServerID int64 `json:"server_id"`
	// Force switches while the primary is offline: what it had not copied
	// yet is lost.
	Force bool `json:"force"`
}

// holdSiteSync waits until no copy of the site's files runs and keeps any
// from starting until release is called.
func (s *Server) holdSiteSync(ctx context.Context, id int64) (release func(), err error) {
	for {
		s.syncs.mu.Lock()
		if s.syncs.running == nil {
			s.syncs.running = map[int64]bool{}
		}
		if !s.syncs.running[id] {
			s.syncs.running[id] = true
			s.syncs.mu.Unlock()
			return func() {
				s.syncs.mu.Lock()
				delete(s.syncs.running, id)
				s.syncs.mu.Unlock()
			}, nil
		}
		s.syncs.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// handleSetSitePrimary makes a server serving a copy of a site its primary.
// With the primary online the hand-over loses nothing: its files are
// copied a last time, its databases only read while the copy catches up.
// Offline (force), the copy takes over as it is.
func (s *Server) handleSetSitePrimary(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req sitePrimaryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !slices.Contains(site.Replicas, req.ServerID) {
		writeError(w, http.StatusUnprocessableEntity, "the server does not serve a copy of the site")
		return
	}
	if !s.agents.Connected(req.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	ctx := r.Context()
	dbs, err := s.db.Databases(ctx, site.ID)
	if err != nil {
		s.internalError(w, r, "list databases", err)
		return
	}
	oldOnline := s.agents.Connected(site.ServerID)
	// No round of database replication meanwhile (one asked for then runs
	// after).
	s.dbReplRun.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			s.dbReplRun.Unlock()
		}
	}
	defer unlock()
	var lag int64 = -1
	if len(dbs) > 0 {
		rows, err := s.db.DBReplications(ctx)
		if err != nil {
			s.internalError(w, r, "load database replication", err)
			return
		}
		var row *store.DBReplication
		for i := range rows {
			if rows[i].ReplicaID == req.ServerID && rows[i].SourceID == site.ServerID {
				row = &rows[i]
			}
		}
		for _, d := range dbs {
			if row == nil || !slices.Contains(row.Databases, d.Name) {
				writeError(w, http.StatusConflict, "the server has no complete copy of the databases yet ("+d.Name+")")
				return
			}
		}
		lag = row.Lag
	}
	if !oldOnline && !req.Force {
		msg := "the primary server is offline: switching keeps what the copy has, and changes it had not copied are lost"
		if lag > 0 {
			msg += " (the databases were " + strconv.FormatInt(lag, 10) + " s behind)"
		}
		writeError(w, http.StatusConflict, msg+"; confirm to switch anyway")
		return
	}
	hand := agentproto.DBHandover{Source: site.ServerID}
	for _, d := range dbs {
		hand.Databases = append(hand.Databases, agentproto.DBCopy{Name: d.Name, User: d.Username})
	}
	release, err := s.holdSiteSync(ctx, site.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "a copy of the files is still running")
		return
	}
	defer release()
	call := func(server int64, typ string, body, resp any, d time.Duration) error {
		cctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return s.agents.Call(cctx, server, typ, body, resp)
	}
	if oldOnline {
		cctx, cancel := context.WithTimeout(ctx, siteSyncTimeout)
		_, err := s.copySite(cctx, site.ID, site.ServerID, req.ServerID)
		cancel()
		if err != nil {
			writeError(w, http.StatusBadGateway, "the last copy of the files failed: "+err.Error())
			return
		}
		if len(dbs) > 0 {
			var demoted agentproto.DBDemoteResponse
			if err := call(site.ServerID, agentproto.TypeDBDemote, hand, &demoted, time.Minute); err != nil {
				writeAgentError(w, err)
				return
			}
			hand.WaitPos = demoted.Pos
		}
	}
	if len(dbs) > 0 {
		if err := call(req.ServerID, agentproto.TypeDBPromote, hand, nil, 6*time.Minute); err != nil {
			if oldOnline {
				// The primary stays: its databases writable again.
				undo := agentproto.DBHandover{Source: req.ServerID, Databases: hand.Databases}
				if uerr := call(site.ServerID, agentproto.TypeDBPromote, undo, nil, time.Minute); uerr != nil {
					s.log.Error("give the primary its databases back", "site_id", site.ID, "err", uerr)
				}
			}
			writeAgentError(w, err)
			return
		}
	}
	old := site.ServerID
	if err := s.db.SetSitePrimary(ctx, site.ID, req.ServerID); err != nil {
		s.internalError(w, r, "set site primary", err)
		return
	}
	s.audit(r, &p.user.ID, "site.primary_changed", map[string]any{"site_id": site.ID, "from": old, "to": req.ServerID, "forced": !oldOnline})
	site, err = s.db.SiteByID(ctx, site.ID)
	if err != nil {
		s.internalError(w, r, "load site", err)
		return
	}
	// The new primary uses its own databases; the former one, back or not,
	// reaches them through the tunnel.
	var problems []string
	if err := s.ensureSiteOn(ctx, site, req.ServerID); err != nil {
		problems = append(problems, err.Error())
	}
	if oldOnline {
		if err := s.ensureSiteOn(ctx, site, old); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, id := range []int64{old, req.ServerID} {
		s.PushProxy(ctx, id)
		if err := s.PushSFTP(ctx, id); err != nil {
			s.log.Warn("push sftp accounts", "server_id", id, "err", err)
		}
	}
	unlock()
	s.startDBReplication()
	s.writeSite(w, r, http.StatusOK, site.ID, "warning", strings.Join(problems, "; "))
}
