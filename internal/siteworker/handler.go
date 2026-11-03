package siteworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// Messages a worker serves, besides MsgFiles (the file manager's requests).
// They are what the agent itself does in a site's folder: a copy of the
// site between servers, an application put in place, a backup restored as
// a copy. A worker serves them with the site user's rights; so the agent
// needs none over the sites' files.
const (
	// MsgManifest lists the site's files and folders for a copy
	// (agentproto.SiteManifestRequest, streaming agentproto.ManifestFrame →
	// agentproto.SiteManifestResponse); MsgSyncRead and MsgSyncWrite read
	// and write them (agentproto.SiteSyncReadRequest →
	// agentproto.SiteSyncReadResponse; agentproto.SiteSyncWriteRequest).
	MsgManifest  = "manifest"
	MsgSyncRead  = "sync_read"
	MsgSyncWrite = "sync_write"
	// MsgCount counts the files under a folder (Count → CountResult).
	MsgCount = "count"
	// MsgExtract unpacks an archive the agent sends in chunks (Extract →
	// ExtractResult).
	MsgExtract = "extract"
	// MsgSymlink makes a link that stays in the site (Symlink).
	MsgSymlink = "symlink"
)

type Symlink struct {
	Target string `json:"target"`
	Path   string `json:"path"`
}

type Count struct {
	Path string `json:"path"`
}

type CountResult struct {
	Files int `json:"files"`
}

// Extract operations: begin an extraction (Kind, Dest, Prefix or Name,
// Limits), send it a chunk (ID, Data), end it (ID: the result) or abort it.
const (
	ExtractBegin = "begin"
	ExtractChunk = "chunk"
	ExtractEnd   = "end"
	ExtractAbort = "abort"
)

// Kinds of archive: a tar stream (into Dest; with Prefix, only what is
// under it, without it), or a single file named Name.
const (
	ExtractTar  = "tar"
	ExtractFile = "file"
)

type Extract struct {
	Op     string       `json:"op"`
	ID     string       `json:"id,omitempty"`
	Kind   string       `json:"kind,omitempty"`
	Dest   string       `json:"dest,omitempty"`
	Prefix string       `json:"prefix,omitempty"`
	Name   string       `json:"name,omitempty"`
	Limits files.Limits `json:"limits"`
	Data   []byte       `json:"data,omitempty"`
}

type ExtractResult struct {
	ID     string               `json:"id,omitempty"`
	Result *files.ExtractResult `json:"result,omitempty"`
}

// maxExtracts bounds the extractions in progress in one worker.
const maxExtracts = 8

// Handler serves a worker's requests for one site; the agent runs the same
// handler in its own process without isolation.
type Handler struct {
	*Files

	mu       sync.Mutex
	extracts map[string]*extraction
}

type extraction struct {
	w    *io.PipeWriter
	done chan struct{}
	res  *files.ExtractResult
	err  error
}

// NewHandler serves a site whose files open gives access to.
func NewHandler(open func() (*files.Site, error)) *Handler {
	return &Handler{Files: NewFiles(open), extracts: map[string]*extraction{}}
}

func decode[T any](payload json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(payload, &v)
	return v, err
}

// Handle serves one request (an agentproto.Handler). Errors carry paths
// relative to the site only.
func (h *Handler) Handle(ctx context.Context, msgType string, payload json.RawMessage) (any, error) {
	switch msgType {
	case MsgFiles:
		req, err := decode[agentproto.FilesRequest](payload)
		if err != nil {
			return nil, err
		}
		return h.Files.Handle(ctx, req)
	case MsgExtract:
		req, err := decode[Extract](payload)
		if err != nil {
			return nil, err
		}
		res, err := h.extract(req)
		return res, files.PublicError(err)
	}
	s, err := h.open()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	switch msgType {
	case MsgManifest:
		if _, err := decode[agentproto.SiteManifestRequest](payload); err != nil {
			return nil, err
		}
		send := agentproto.StreamSender(ctx)
		if send == nil {
			return nil, errors.New("a manifest is a streaming request")
		}
		resp, err := Manifest(ctx, s, send)
		return resp, files.PublicError(err)
	case MsgSyncRead:
		req, err := decode[agentproto.SiteSyncReadRequest](payload)
		if err != nil {
			return nil, err
		}
		resp, err := SyncRead(s, req)
		return resp, files.PublicError(err)
	case MsgSyncWrite:
		req, err := decode[agentproto.SiteSyncWriteRequest](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, files.PublicError(SyncWrite(s, req))
	case MsgCount:
		req, err := decode[Count](payload)
		if err != nil {
			return nil, err
		}
		n, err := CountFiles(s, req.Path)
		return CountResult{Files: n}, files.PublicError(err)
	case MsgSymlink:
		req, err := decode[Symlink](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, files.PublicError(s.Symlink(req.Target, req.Path))
	}
	return nil, fmt.Errorf("unknown message %q", msgType)
}

// CountFiles counts the files (not folders) under p.
func CountFiles(s *files.Site, p string) (int, error) {
	c, err := files.Clean(p)
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.Walk(func(q string, _ bool) bool {
		// Only the folder p and what is under it.
		return c != "." && q != c && !isUnder(q, c) && !isUnder(c, q)
	}, func(e files.WalkEntry) error {
		if !e.Dir && isUnder(e.Path, c) {
			n++
		}
		return nil
	})
	return n, err
}

// isUnder reports whether p is inside folder dir (both clean, relative).
func isUnder(p, dir string) bool {
	return dir == "." || len(p) > len(dir) && p[:len(dir)] == dir && p[len(dir)] == '/'
}

func (h *Handler) extract(req Extract) (ExtractResult, error) {
	switch req.Op {
	case ExtractBegin:
		return h.beginExtract(req)
	case ExtractChunk:
		x, err := h.extraction(req.ID, false)
		if err != nil {
			return ExtractResult{}, err
		}
		if _, err := x.w.Write(req.Data); err != nil {
			// The extraction failed: its error, not the pipe's.
			x, _ := h.extraction(req.ID, true)
			<-x.done
			return ExtractResult{}, x.err
		}
		return ExtractResult{ID: req.ID}, nil
	case ExtractEnd, ExtractAbort:
		x, err := h.extraction(req.ID, true)
		if err != nil {
			return ExtractResult{}, err
		}
		if req.Op == ExtractAbort {
			x.w.CloseWithError(errors.New("aborted"))
			<-x.done
			return ExtractResult{}, nil
		}
		x.w.Close()
		<-x.done
		return ExtractResult{Result: x.res}, x.err
	}
	return ExtractResult{}, fmt.Errorf("unknown extract operation %q", req.Op)
}

func (h *Handler) beginExtract(req Extract) (ExtractResult, error) {
	if req.Limits.MaxFiles <= 0 || req.Limits.MaxTotalBytes <= 0 {
		return ExtractResult{}, errors.New("an extraction needs limits")
	}
	if req.Kind != ExtractTar && req.Kind != ExtractFile {
		return ExtractResult{}, fmt.Errorf("unknown archive kind %q", req.Kind)
	}
	h.mu.Lock()
	n := len(h.extracts)
	h.mu.Unlock()
	if n >= maxExtracts {
		return ExtractResult{}, errors.New("too many extractions in progress")
	}
	s, err := h.open()
	if err != nil {
		return ExtractResult{}, err
	}
	pr, pw := io.Pipe()
	x := &extraction{w: pw, done: make(chan struct{})}
	go func() {
		defer close(x.done)
		defer s.Close()
		switch {
		case req.Kind == ExtractFile:
			x.res, x.err = s.ExtractFile(pr, req.Dest, req.Name, req.Limits)
		case req.Prefix != "":
			x.res, x.err = s.ExtractTarStrip(pr, req.Dest, req.Prefix, req.Limits)
		default:
			x.res, x.err = s.ExtractTar(pr, req.Dest, req.Limits)
		}
		if x.err != nil {
			pr.CloseWithError(x.err)
			return
		}
		io.Copy(io.Discard, pr) // the end of the archive
	}()
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	h.mu.Lock()
	h.extracts[id] = x
	h.mu.Unlock()
	return ExtractResult{ID: id}, nil
}

func (h *Handler) extraction(id string, take bool) (*extraction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	x := h.extracts[id]
	if x == nil {
		return nil, errors.New("unknown extraction")
	}
	if take {
		delete(h.extracts, id)
	}
	return x, nil
}

// Close abandons the uploads and extractions in progress.
func (h *Handler) Close() {
	h.Files.Close()
	h.mu.Lock()
	xs := h.extracts
	h.extracts = map[string]*extraction{}
	h.mu.Unlock()
	for _, x := range xs {
		x.w.CloseWithError(errors.New("aborted"))
		<-x.done
	}
}
