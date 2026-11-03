// Package siteworker is the file access of one site, run as the site's own
// system user: the agent's privileged helper starts "basalt site-worker"
// with the site's uid and gid on a socket the agent hands it, and the agent
// sends it the panel's file requests, its own work in the site's folder
// (Handler) or an SFTP session. A path bug here can then reach no further
// than the site user could on its own; the kernel, not just os.Root, keeps
// other sites and the host out. The agent itself has no rights over the
// sites' files.
//
// Without isolation (development) the agent runs the same Handler in its
// own process.
package siteworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// UploadIdleTimeout is how long an upload may go without a chunk before it
// is abandoned (browser closed, say).
const UploadIdleTimeout = time.Hour

// Files serves the file manager's requests for one site.
type Files struct {
	open func() (*files.Site, error)

	mu      sync.Mutex
	uploads map[string]*pendingUpload
}

type pendingUpload struct {
	site     *files.Site
	upload   *files.Upload
	lastUsed time.Time
}

// NewFiles serves a site whose files open gives access to.
func NewFiles(open func() (*files.Site, error)) *Files {
	return &Files{open: open, uploads: map[string]*pendingUpload{}}
}

// Handle performs one request. Errors carry paths relative to the site only.
func (f *Files) Handle(ctx context.Context, req agentproto.FilesRequest) (*agentproto.FilesResponse, error) {
	switch req.Op {
	case agentproto.FilesUploadChunk:
		u, err := f.upload(req.UploadID)
		if err != nil {
			return nil, err
		}
		if err := u.upload.Write(req.Offset, req.Data); err != nil {
			return nil, files.PublicError(err)
		}
		return &agentproto.FilesResponse{Size: u.upload.Size()}, nil
	case agentproto.FilesUploadCommit, agentproto.FilesUploadAbort:
		u, err := f.take(req.UploadID)
		if err != nil {
			return nil, err
		}
		defer u.site.Close()
		if req.Op == agentproto.FilesUploadAbort {
			u.upload.Abort()
			return &agentproto.FilesResponse{}, nil
		}
		size := u.upload.Size()
		if err := u.upload.Commit(); err != nil {
			return nil, files.PublicError(err)
		}
		return &agentproto.FilesResponse{Size: size}, nil
	}

	s, err := f.open()
	if err != nil {
		return nil, err
	}
	if req.Op == agentproto.FilesUploadBegin {
		u, err := s.BeginUpload(req.Path)
		if err != nil {
			s.Close()
			return nil, files.PublicError(err)
		}
		return &agentproto.FilesResponse{UploadID: f.add(s, u)}, nil
	}
	defer s.Close()

	resp := &agentproto.FilesResponse{}
	switch req.Op {
	case agentproto.FilesList:
		var pg files.Page
		pg, err = s.ListPage(req.Path, req.Cursor, req.Limit)
		resp.Entries, resp.Total, resp.Next = pg.Entries, pg.Total, pg.Next
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
				err = fmt.Errorf("%s: %w", p, files.PublicError(err))
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
		return nil, files.PublicError(err)
	}
	return resp, nil
}

func (f *Files) add(s *files.Site, u *files.Upload) string {
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	f.mu.Lock()
	f.uploads[id] = &pendingUpload{site: s, upload: u, lastUsed: time.Now()}
	f.mu.Unlock()
	return id
}

var errUnknownUpload = errors.New("unknown or expired upload")

func (f *Files) upload(id string) (*pendingUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if u == nil {
		return nil, errUnknownUpload
	}
	u.lastUsed = time.Now()
	return u, nil
}

func (f *Files) take(id string) (*pendingUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if u == nil {
		return nil, errUnknownUpload
	}
	delete(f.uploads, id)
	return u, nil
}

// Expire abandons uploads without a chunk for idle.
func (f *Files) Expire(idle time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, u := range f.uploads {
		if time.Since(u.lastUsed) > idle {
			u.upload.Abort()
			u.site.Close()
			delete(f.uploads, id)
		}
	}
}

// Uploads is the number of uploads in progress.
func (f *Files) Uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// Close abandons every upload in progress.
func (f *Files) Close() { f.Expire(-1) }
