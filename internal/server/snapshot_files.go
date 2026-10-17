package server

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// Single files and folders of a backup of a site's files: browse a
// snapshot, download a file (or a folder as a zip archive), and restore a
// copy into a new folder of the site without touching anything else.

const (
	exportTimeout      = time.Hour
	restoreCopyTimeout = 6 * time.Hour
)

var snapshotParamRE = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// snapshotFiles reads the site, snapshot and path of a request and the
// repository; it writes the error response itself.
func (s *Server) snapshotFiles(w http.ResponseWriter, r *http.Request, p string) (*backupTarget, agentproto.SnapshotFilesRequest, bool) {
	t, ok := s.pathSiteTarget(w, r)
	if !ok {
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	snap := r.PathValue("sid")
	if !snapshotParamRE.MatchString(snap) {
		writeError(w, http.StatusNotFound, "backup not found")
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	p = strings.Trim(p, "/")
	if !agentproto.ValidSnapshotPath(p) {
		writeError(w, http.StatusUnprocessableEntity, "invalid path")
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	if !s.backupsAvailable(w, r, t.ServerID) {
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	repo, ok := s.targetRepo(w, r, t)
	if !ok {
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	return t, agentproto.SnapshotFilesRequest{Repo: repo, Resource: t.resource(), SiteID: t.SiteID, Snapshot: snap, Path: p}, true
}

func (s *Server) handleSnapshotFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	t, req, ok := s.snapshotFiles(w, r, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupCheckTime)
	defer cancel()
	var resp agentproto.BrowseResponse
	if err := s.agents.Call(ctx, t.ServerID, agentproto.TypeBackupBrowse, req, &resp); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": resp.Path, "entries": nonNil(resp.Entries)})
}

// handleSnapshotDownload has the server write the file (or a zip of the
// folder) out of the backup, then streams it from there in chunks. Like any
// download of user content, it is an attachment with a sandboxing CSP.
func (s *Server) handleSnapshotDownload(w http.ResponseWriter, r *http.Request, p *principal) {
	t, req, ok := s.snapshotFiles(w, r, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
	defer cancel()
	var exp agentproto.ExportResponse
	// Streaming, so the long export does not hold one of the agent's few
	// slots for ordinary requests.
	if err := s.agents.Stream(ctx, t.ServerID, agentproto.TypeBackupExport, req, &exp, func(json.RawMessage) error { return nil }); err != nil {
		writeAgentError(w, err)
		return
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), fileOpTimeout)
		defer ccancel()
		if err := s.agents.Call(cctx, t.ServerID, agentproto.TypeBackupExportClose, agentproto.ExportClose{ID: exp.ID}, nil); err != nil {
			s.log.Warn("drop exported backup file", "site_id", t.SiteID, "err", err)
		}
	}()
	s.audit(r, &p.user.ID, "backup.file_downloaded", map[string]any{
		"resource": t.resource(), "snapshot": req.Snapshot, "path": "/" + req.Path, "bytes": exp.Size,
	})

	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transferDeadline))
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": exp.Name}))
	h.Set("Content-Length", strconv.FormatInt(exp.Size, 10))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for off := int64(0); off < exp.Size; {
		var chunk agentproto.ExportChunk
		cctx, ccancel := context.WithTimeout(r.Context(), fileOpTimeout)
		err := s.agents.Call(cctx, t.ServerID, agentproto.TypeBackupExportRead,
			agentproto.ExportRead{ID: exp.ID, Offset: off, Length: files.MaxChunk}, &chunk)
		ccancel()
		if err != nil || len(chunk.Data) == 0 {
			// Headers are sent; the short body tells the client it failed.
			s.log.Warn("backup download interrupted", "site_id", t.SiteID, "err", err)
			return
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		off += int64(len(chunk.Data))
	}
}

type restoreCopyRequest struct {
	Path string `json:"path"`
}

// handleRestoreCopy restores a file or folder of a backup into a new
// folder "restored-<time>" of the site; nothing else changes.
func (s *Server) handleRestoreCopy(w http.ResponseWriter, r *http.Request, p *principal) {
	var body restoreCopyRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	t, req, ok := s.snapshotFiles(w, r, body.Path)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), restoreCopyTimeout)
	defer cancel()
	var resp agentproto.RestoreCopyResponse
	if err := s.agents.Stream(ctx, t.ServerID, agentproto.TypeBackupRestoreCopy, req, &resp, func(json.RawMessage) error { return nil }); err != nil {
		writeAgentError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "backup.copy_restored", map[string]any{
		"resource": t.resource(), "snapshot": req.Snapshot, "path": "/" + req.Path, "folder": resp.Folder, "files": resp.Files,
	})
	writeJSON(w, http.StatusCreated, resp)
}
