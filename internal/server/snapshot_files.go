package server

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// Single files and folders of a backup: of a site's files or of an app's
// volume. Browse a snapshot, download a file (or a folder as a zip
// archive), restore a copy into a new folder of the site or volume without
// touching anything else, and download a database dump.

const (
	exportTimeout      = time.Hour
	restoreCopyTimeout = 6 * time.Hour
)

var snapshotParamRE = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// snapshotFiles reads the target, snapshot, part (volume or database) and
// path of a request and the repository; it writes the error response
// itself.
func (s *Server) snapshotFiles(w http.ResponseWriter, r *http.Request, resolve targetResolver, volume, database, p string) (*backupTarget, agentproto.SnapshotFilesRequest, bool) {
	t, ok := resolve(w, r)
	if !ok {
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	snap := r.PathValue("sid")
	if !snapshotParamRE.MatchString(snap) {
		writeError(w, http.StatusNotFound, "backup not found")
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	req := agentproto.SnapshotFilesRequest{Resource: t.resource(), Snapshot: snap, Path: strings.Trim(p, "/")}
	switch {
	case t.site != nil && volume != "":
		writeError(w, http.StatusUnprocessableEntity, "a site has no volumes")
		return nil, agentproto.SnapshotFilesRequest{}, false
	case t.site != nil:
		req.SiteID, req.Part = t.site.ID, agentproto.PartFiles
		if database != "" {
			req.Part = agentproto.PartMariaDB + database
		}
	case volume != "" && database != "", volume == "" && database == "":
		writeError(w, http.StatusUnprocessableEntity, "choose a volume or a database of the app")
		return nil, agentproto.SnapshotFilesRequest{}, false
	default:
		req.AppID, req.Part = t.app.ID, agentproto.PartVolume+volume
		if database != "" {
			req.Part = agentproto.PartDump + database
		}
	}
	if !agentproto.ValidSnapshotPath(req.Path) || (req.IsDump() && req.Path != "") {
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
	req.Repo = repo
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, agentproto.SnapshotFilesRequest{}, false
	}
	return t, req, true
}

// partDetails describes a part of a backup for the audit log.
func partDetails(t *backupTarget, req agentproto.SnapshotFilesRequest) map[string]any {
	return map[string]any{"resource": t.resource(), "part": req.PartOf(), "snapshot": req.Snapshot, "path": "/" + req.Path}
}

func (s *Server) handleSnapshotFiles(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		q := r.URL.Query()
		t, req, ok := s.snapshotFiles(w, r, resolve, q.Get("volume"), "", q.Get("path"))
		if !ok {
			return
		}
		// Reading a volume's archive takes as long as the volume is large
		// (once: the agent keeps the listing).
		ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
		defer cancel()
		var resp agentproto.BrowseResponse
		if err := s.agents.Stream(ctx, t.ServerID, agentproto.TypeBackupBrowse, req, &resp, func(json.RawMessage) error { return nil }); err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": resp.Path, "entries": nonNil(resp.Entries)})
	}
}

func (s *Server) handleSnapshotDownload(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		q := r.URL.Query()
		t, req, ok := s.snapshotFiles(w, r, resolve, q.Get("volume"), q.Get("database"), q.Get("path"))
		if ok {
			s.snapshotDownload(w, r, p, t, req)
		}
	}
}

func (s *Server) snapshotDownload(w http.ResponseWriter, r *http.Request, p *principal, t *backupTarget, req agentproto.SnapshotFilesRequest) {
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
			s.log.Warn("drop exported backup file", "resource", t.resource(), "err", err)
		}
	}()
	details := partDetails(t, req)
	details["bytes"] = exp.Size
	s.audit(r, &p.user.ID, "backup.file_downloaded", details)

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
			s.log.Warn("backup download interrupted", "resource", t.resource(), "err", err)
			return
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		off += int64(len(chunk.Data))
	}
}

type restoreCopyRequest struct {
	Path   string `json:"path"`
	Volume string `json:"volume"` // apps: the volume
}

// handleRestoreCopy restores a file or folder of a backup into a new folder
// "restored-<time>" of the site or volume; nothing else changes.
func (s *Server) handleRestoreCopy(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		var body restoreCopyRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		t, req, ok := s.snapshotFiles(w, r, resolve, body.Volume, "", body.Path)
		if ok {
			s.restoreCopy(w, r, p, t, req, body.Volume)
		}
	}
}

func (s *Server) restoreCopy(w http.ResponseWriter, r *http.Request, p *principal, t *backupTarget, req agentproto.SnapshotFilesRequest, volume string) {
	if volume != "" {
		items, err := s.items(r.Context(), t)
		if err != nil {
			s.internalError(w, r, "list backup items", err)
			return
		}
		if !slices.ContainsFunc(items, func(it agentproto.BackupItem) bool { return it.Kind == agentproto.ItemVolume && it.Volume == volume }) {
			writeError(w, http.StatusUnprocessableEntity, "the app has no volume "+volume+" now")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), restoreCopyTimeout)
	defer cancel()
	var resp agentproto.RestoreCopyResponse
	if err := s.agents.Stream(ctx, t.ServerID, agentproto.TypeBackupRestoreCopy, req, &resp, func(json.RawMessage) error { return nil }); err != nil {
		writeAgentError(w, err)
		return
	}
	details := partDetails(t, req)
	details["folder"], details["files"] = resp.Folder, resp.Files
	s.audit(r, &p.user.ID, "backup.copy_restored", details)
	writeJSON(w, http.StatusCreated, resp)
}
