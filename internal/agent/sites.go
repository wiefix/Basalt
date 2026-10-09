package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

const (
	uploadIdleTimeout = time.Hour
	trashDir          = ".trash"
)

var siteDirs = []string{"public", "logs", "tmp"}

const placeholderPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Site is ready</title></head>
<body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>It works!</h1><p>Upload your files to replace this page.</p></body></html>
`

// sitesManager owns the per-site directories under SitesRoot:
//
//	<SitesRoot>/<id>/public   served by the proxy
//	<SitesRoot>/<id>/logs
//	<SitesRoot>/<id>/tmp
type sitesManager struct {
	log  *slog.Logger
	root string

	mu      sync.Mutex
	uploads map[string]*pendingUpload
}

type pendingUpload struct {
	siteID   int64
	site     *files.Site
	upload   *files.Upload
	lastUsed time.Time
}

func newSitesManager(root string, log *slog.Logger) *sitesManager {
	return &sitesManager{log: log, root: root, uploads: map[string]*pendingUpload{}}
}

func (m *sitesManager) dir(id int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("invalid site id %d", id)
	}
	return filepath.Join(m.root, strconv.FormatInt(id, 10)), nil
}

// PublicDir is where the proxy serves a site from.
func (m *sitesManager) PublicDir(id int64) string {
	d, _ := m.dir(id)
	return filepath.Join(d, "public")
}

// Ensure creates the site's directories (idempotent). A new, empty public
// directory gets a placeholder page.
func (m *sitesManager) Ensure(id int64) error {
	d, err := m.dir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(d); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", d)
	}
	for _, sub := range siteDirs {
		if err := os.MkdirAll(filepath.Join(d, sub), 0o755); err != nil {
			return err
		}
	}
	pub := filepath.Join(d, "public")
	entries, err := os.ReadDir(pub)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := os.WriteFile(filepath.Join(pub, "index.html"), []byte(placeholderPage), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Delete moves the site to the trash directory rather than erasing it, so an
// accidental deletion can be recovered by an operator.
func (m *sitesManager) Delete(id int64) error {
	d, err := m.dir(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	m.abortUploads(id)
	trash := filepath.Join(m.root, trashDir)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(trash, fmt.Sprintf("%d-%d", id, time.Now().Unix()))
	if err := os.Rename(d, dest); err != nil {
		return err
	}
	m.log.Info("site moved to trash", "site_id", id, "path", dest)
	return nil
}

func (m *sitesManager) open(id int64) (*files.Site, error) {
	d, err := m.dir(id)
	if err != nil {
		return nil, err
	}
	s, err := files.Open(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("site %d does not exist on this server", id)
	}
	return s, err
}

func (m *sitesManager) handleFiles(ctx context.Context, req agentproto.FilesRequest) (*agentproto.FilesResponse, error) {
	switch req.Op {
	case agentproto.FilesUploadChunk:
		u, err := m.upload(req.SiteID, req.UploadID)
		if err != nil {
			return nil, err
		}
		if err := u.upload.Write(req.Offset, req.Data); err != nil {
			return nil, err
		}
		return &agentproto.FilesResponse{Size: u.upload.Size()}, nil
	case agentproto.FilesUploadCommit, agentproto.FilesUploadAbort:
		u, err := m.takeUpload(req.SiteID, req.UploadID)
		if err != nil {
			return nil, err
		}
		defer u.site.Close()
		if req.Op == agentproto.FilesUploadAbort {
			u.upload.Abort()
			return &agentproto.FilesResponse{}, nil
		}
		size := u.upload.Size()
		return &agentproto.FilesResponse{Size: size}, u.upload.Commit()
	}

	s, err := m.open(req.SiteID)
	if err != nil {
		return nil, err
	}
	if req.Op == agentproto.FilesUploadBegin {
		u, err := s.BeginUpload(req.Path)
		if err != nil {
			s.Close()
			return nil, err
		}
		id := m.addUpload(req.SiteID, s, u)
		return &agentproto.FilesResponse{UploadID: id}, nil
	}
	defer s.Close()

	resp := &agentproto.FilesResponse{}
	switch req.Op {
	case agentproto.FilesList:
		resp.Entries, err = s.List(req.Path)
	case agentproto.FilesStat:
		var e files.Entry
		e, err = s.Stat(req.Path)
		resp.Entry = &e
	case agentproto.FilesRead:
		resp.Data, resp.Size, err = s.Read(req.Path, req.Offset, req.Length)
	case agentproto.FilesWrite:
		err = s.WriteFile(req.Path, req.Data)
	case agentproto.FilesMkdir:
		err = s.Mkdir(req.Path)
	case agentproto.FilesRename:
		err = s.Rename(req.Path, req.To)
	case agentproto.FilesDelete:
		for _, p := range req.Paths {
			if err = s.Delete(p); err != nil {
				err = fmt.Errorf("%s: %w", p, err)
				break
			}
		}
	case agentproto.FilesChmod:
		err = s.Chmod(req.Path, req.Mode)
	case agentproto.FilesExtract:
		resp.Extract, err = s.Extract(req.Path, req.To, req.Overwrite, files.DefaultLimits)
	case agentproto.FilesCompress:
		err = s.Compress(req.Paths, req.To)
	default:
		err = fmt.Errorf("unknown files operation %q", req.Op)
	}
	if err != nil {
		return nil, publicError(err)
	}
	return resp, nil
}

// publicError strips absolute host paths from errors before they leave the
// agent; the panel only needs to see paths relative to the site.
func publicError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s: %w", le.Op, le.Err)
	}
	return err
}

func (m *sitesManager) addUpload(siteID int64, s *files.Site, u *files.Upload) string {
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	m.mu.Lock()
	m.uploads[id] = &pendingUpload{siteID: siteID, site: s, upload: u, lastUsed: time.Now()}
	m.mu.Unlock()
	return id
}

func (m *sitesManager) upload(siteID int64, id string) (*pendingUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.uploads[id]
	if u == nil || u.siteID != siteID {
		return nil, errors.New("unknown or expired upload")
	}
	u.lastUsed = time.Now()
	return u, nil
}

func (m *sitesManager) takeUpload(siteID int64, id string) (*pendingUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.uploads[id]
	if u == nil || u.siteID != siteID {
		return nil, errors.New("unknown or expired upload")
	}
	delete(m.uploads, id)
	return u, nil
}

func (m *sitesManager) abortUploads(siteID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, u := range m.uploads {
		if u.siteID == siteID {
			u.upload.Abort()
			u.site.Close()
			delete(m.uploads, id)
		}
	}
}

// janitor aborts uploads abandoned by the panel (e.g. browser closed).
func (m *sitesManager) janitor(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		for id, u := range m.uploads {
			if time.Since(u.lastUsed) > uploadIdleTimeout {
				u.upload.Abort()
				u.site.Close()
				delete(m.uploads, id)
			}
		}
		m.mu.Unlock()
	}
}
