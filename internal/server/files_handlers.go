package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/store"
)

const (
	maxUploadBytes   = 4 << 30
	fileOpTimeout    = 30 * time.Second
	longOpTimeout    = 15 * time.Minute // extract/compress
	transferDeadline = 2 * time.Hour
)

// filesCall runs one file-manager operation on the site's agent.
func (s *Server) filesCall(ctx context.Context, site *store.Site, req agentproto.FilesRequest, timeout time.Duration) (*agentproto.FilesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req.SiteID = site.ID
	var resp agentproto.FilesResponse
	if err := s.agents.Call(ctx, site.ServerID, agentproto.TypeFiles, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// writeAgentError maps agent failures to HTTP responses. Errors coming from
// the agent's file operations are shown to the user; they contain only
// site-relative paths.
func writeAgentError(w http.ResponseWriter, err error) {
	var re *agentproto.RemoteError
	switch {
	case errors.As(err, &re):
		status := http.StatusUnprocessableEntity
		switch {
		case strings.Contains(re.Msg, "no such file"), strings.Contains(re.Msg, "file does not exist"):
			status = http.StatusNotFound
		case strings.Contains(re.Msg, files.ErrExists.Error()):
			status = http.StatusConflict
		}
		writeError(w, status, re.Msg)
	default:
		writeError(w, http.StatusBadGateway, "server unreachable: "+err.Error())
	}
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	resp, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{Op: agentproto.FilesList, Path: dir}, fileOpTimeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	clean, _ := files.Clean(dir)
	entries := resp.Entries
	if entries == nil {
		entries = []files.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": "/" + strings.TrimPrefix(clean, "."), "entries": entries})
}

func (s *Server) handleStatFile(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	resp, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{Op: agentproto.FilesStat, Path: r.URL.Query().Get("path")}, fileOpTimeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": resp.Entry})
}

// handleDownload streams a file. It is always served as an attachment with a
// sandboxing CSP: user content must never execute in the panel's origin.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	p0 := r.URL.Query().Get("path")
	first, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{Op: agentproto.FilesRead, Path: p0, Length: files.MaxChunk}, fileOpTimeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transferDeadline))

	name := path.Base("/" + p0)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	h.Set("Content-Length", strconv.FormatInt(first.Size, 10))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	off := int64(len(first.Data))
	if _, err := w.Write(first.Data); err != nil {
		return
	}
	for off < first.Size {
		chunk, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{
			Op: agentproto.FilesRead, Path: p0, Offset: off, Length: files.MaxChunk,
		}, fileOpTimeout)
		if err != nil || len(chunk.Data) == 0 {
			// Headers are sent; the short body tells the client it failed.
			s.log.Warn("download interrupted", "site_id", site.ID, "err", err)
			return
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		off += int64(len(chunk.Data))
	}
}

// handleUpload streams the raw request body into a file, in chunks, through
// an atomic upload on the agent. The file appears only when complete.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	dest := r.URL.Query().Get("path")
	if r.ContentLength > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(transferDeadline))
	rc.SetWriteDeadline(time.Now().Add(transferDeadline))
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)

	begin, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{Op: agentproto.FilesUploadBegin, Path: dest}, fileOpTimeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	abort := func() {
		s.filesCall(context.WithoutCancel(r.Context()), site,
			agentproto.FilesRequest{Op: agentproto.FilesUploadAbort, UploadID: begin.UploadID}, fileOpTimeout)
	}

	buf := make([]byte, files.MaxChunk)
	var off int64
	for {
		n, rerr := io.ReadFull(body, buf)
		if n > 0 {
			if _, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{
				Op: agentproto.FilesUploadChunk, UploadID: begin.UploadID, Offset: off, Data: buf[:n],
			}, fileOpTimeout); err != nil {
				abort()
				writeAgentError(w, err)
				return
			}
			off += int64(n)
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			abort()
			var tooBig *http.MaxBytesError
			if errors.As(rerr, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "file too large")
			} else {
				writeError(w, http.StatusBadRequest, "upload interrupted")
			}
			return
		}
	}
	if _, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{
		Op: agentproto.FilesUploadCommit, UploadID: begin.UploadID,
	}, fileOpTimeout); err != nil {
		writeAgentError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "file.uploaded", map[string]any{"site_id": site.ID, "path": dest, "bytes": off})
	writeJSON(w, http.StatusCreated, map[string]any{"path": dest, "size": off})
}

type fileOpRequest struct {
	Path      string   `json:"path"`
	To        string   `json:"to"`
	Paths     []string `json:"paths"`
	Mode      string   `json:"mode"`
	Content   *string  `json:"content"`
	Overwrite bool     `json:"overwrite"`
}

// handleFileOp serves the small mutating operations, selected by {op} in the URL.
func (s *Server) handleFileOp(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req fileOpRequest
	if !decodeJSONLimit(w, r, &req, files.MaxChunk*2) {
		return
	}
	op := r.PathValue("op")
	ar := agentproto.FilesRequest{Path: req.Path, To: req.To, Paths: req.Paths, Overwrite: req.Overwrite}
	timeout := fileOpTimeout
	switch op {
	case "mkdir":
		ar.Op = agentproto.FilesMkdir
	case "rename":
		ar.Op = agentproto.FilesRename
	case "delete":
		ar.Op = agentproto.FilesDelete
		if len(req.Paths) == 0 || len(req.Paths) > 1000 {
			writeError(w, http.StatusUnprocessableEntity, "paths must contain 1 to 1000 entries")
			return
		}
	case "chmod":
		mode, err := strconv.ParseUint(req.Mode, 8, 32)
		if err != nil || mode > 0o777 {
			writeError(w, http.StatusUnprocessableEntity, "mode must be octal, 000 to 777")
			return
		}
		ar.Op, ar.Mode = agentproto.FilesChmod, uint32(mode)
	case "write":
		if req.Content == nil {
			writeError(w, http.StatusUnprocessableEntity, "content is required")
			return
		}
		ar.Op, ar.Data = agentproto.FilesWrite, []byte(*req.Content)
	case "extract":
		ar.Op, timeout = agentproto.FilesExtract, longOpTimeout
	case "compress":
		ar.Op, timeout = agentproto.FilesCompress, longOpTimeout
		if len(req.Paths) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "paths is required")
			return
		}
	default:
		writeError(w, http.StatusNotFound, "unknown operation")
		return
	}
	if timeout == longOpTimeout {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(longOpTimeout + time.Minute))
	}
	resp, err := s.filesCall(r.Context(), site, ar, timeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "file."+op, map[string]any{
		"site_id": site.ID, "path": req.Path, "to": req.To, "paths": req.Paths, "mode": req.Mode,
	})
	out := map[string]any{"ok": true}
	if resp.Extract != nil {
		out["extract"] = resp.Extract
	}
	writeJSON(w, http.StatusOK, out)
}

// handleReadText returns a small text file for the editor.
func (s *Server) handleReadText(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	resp, err := s.filesCall(r.Context(), site, agentproto.FilesRequest{
		Op: agentproto.FilesRead, Path: r.URL.Query().Get("path"), Length: files.MaxChunk,
	}, fileOpTimeout)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if resp.Size > files.MaxChunk {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file is larger than %d bytes; download it instead", files.MaxChunk))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": string(resp.Data), "size": resp.Size})
}
