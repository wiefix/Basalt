package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/siteworker"
)

// siteFS is the agent's access to a site's files: every operation is a
// request to the site's worker, so with isolation it is done with the site
// user's rights (the agent has none over the sites' files). Paths are
// those of the file manager ("/public/index.html").
type siteFS struct {
	m   *sitesManager
	ctx context.Context
	id  int64
}

// files opens the site's files for the operations of ctx.
func (m *sitesManager) files(ctx context.Context, id int64) (*siteFS, error) {
	if _, err := m.siteDir(id); err != nil {
		return nil, err
	}
	return &siteFS{m: m, ctx: ctx, id: id}, nil
}

// Close: nothing to release (the worker stays for the next request).
func (s *siteFS) Close() error { return nil }

func (s *siteFS) do(req agentproto.FilesRequest) (*agentproto.FilesResponse, error) {
	req.SiteID = s.id
	resp := &agentproto.FilesResponse{}
	if err := s.m.callSite(s.ctx, s.id, siteworker.MsgFiles, req, resp, nil); err != nil {
		return nil, remoteFilesError(err)
	}
	return resp, nil
}

// remoteFilesError gives a worker's error, which arrives as text, back the
// sentinel the agent's code checks for.
func remoteFilesError(err error) error {
	switch msg := err.Error(); {
	case strings.HasSuffix(msg, files.ErrExists.Error()):
		return fmt.Errorf("%w (%s)", files.ErrExists, msg)
	case strings.HasSuffix(msg, fs.ErrNotExist.Error()):
		return fmt.Errorf("%w (%s)", fs.ErrNotExist, msg)
	}
	return err
}

// List is the whole folder p.
func (s *siteFS) List(p string) ([]files.Entry, error) {
	var out []files.Entry
	cursor := ""
	for {
		resp, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesList, Path: p, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Entries...)
		if resp.Next == "" {
			return out, nil
		}
		cursor = resp.Next
	}
}

func (s *siteFS) Stat(p string) (files.Entry, error) {
	resp, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesStat, Path: p})
	if err != nil {
		return files.Entry{}, err
	}
	if resp.Entry == nil {
		return files.Entry{}, errors.New("no entry in the answer")
	}
	return *resp.Entry, nil
}

func (s *siteFS) Read(p string, off int64, n int) ([]byte, int64, error) {
	resp, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesRead, Path: p, Offset: off, Length: n})
	if err != nil {
		return nil, 0, err
	}
	return resp.Data, resp.Size, nil
}

func (s *siteFS) WriteFile(p string, data []byte) error {
	_, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesWrite, Path: p, Data: data})
	return err
}

func (s *siteFS) Mkdir(p string) error {
	_, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesMkdir, Path: p})
	return err
}

func (s *siteFS) Rename(from, to string) error {
	_, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesRename, Path: from, To: to})
	return err
}

func (s *siteFS) Delete(p string) error {
	_, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesDelete, Paths: []string{p}})
	return err
}

func (s *siteFS) Chmod(p string, mode uint32) error {
	_, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesChmod, Path: p, Mode: mode})
	return err
}

// CopyIn writes what r holds to p (replacing a file there), in chunks.
func (s *siteFS) CopyIn(p string, r io.Reader) error {
	resp, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesUploadBegin, Path: p})
	if err != nil {
		return err
	}
	id := resp.UploadID
	buf := make([]byte, files.MaxChunk)
	var off int64
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if _, err := s.do(agentproto.FilesRequest{Op: agentproto.FilesUploadChunk, UploadID: id, Offset: off, Data: buf[:n]}); err != nil {
				s.do(agentproto.FilesRequest{Op: agentproto.FilesUploadAbort, UploadID: id})
				return err
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			s.do(agentproto.FilesRequest{Op: agentproto.FilesUploadAbort, UploadID: id})
			return rerr
		}
	}
	_, err = s.do(agentproto.FilesRequest{Op: agentproto.FilesUploadCommit, UploadID: id})
	return err
}

// extract unpacks what r holds in the worker: a tar stream into dest
// (only what is under prefix, without it, when prefix is set), or a single
// file named name (kind siteworker.ExtractFile).
func (s *siteFS) extract(r io.Reader, x siteworker.Extract) (*files.ExtractResult, error) {
	x.Op = siteworker.ExtractBegin
	var res siteworker.ExtractResult
	if err := s.m.callSite(s.ctx, s.id, siteworker.MsgExtract, x, &res, nil); err != nil {
		return nil, err
	}
	id := res.ID
	abort := func() {
		// With the context of the request gone, still tell the worker.
		s.m.callSite(context.WithoutCancel(s.ctx), s.id, siteworker.MsgExtract, siteworker.Extract{Op: siteworker.ExtractAbort, ID: id}, nil, nil)
	}
	buf := make([]byte, files.MaxChunk)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if err := s.m.callSite(s.ctx, s.id, siteworker.MsgExtract, siteworker.Extract{Op: siteworker.ExtractChunk, ID: id, Data: buf[:n]}, nil, nil); err != nil {
				// A chunk refused ended the extraction (its error); one that
				// did not arrive did not.
				abort()
				return nil, err
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			abort()
			return nil, rerr
		}
	}
	res = siteworker.ExtractResult{}
	if err := s.m.callSite(s.ctx, s.id, siteworker.MsgExtract, siteworker.Extract{Op: siteworker.ExtractEnd, ID: id}, &res, nil); err != nil {
		return nil, err
	}
	if res.Result == nil {
		return nil, errors.New("no result of the extraction")
	}
	return res.Result, nil
}

func (s *siteFS) ExtractTar(r io.Reader, dest string, lim files.Limits) (*files.ExtractResult, error) {
	return s.extract(r, siteworker.Extract{Kind: siteworker.ExtractTar, Dest: dest, Limits: lim})
}

func (s *siteFS) ExtractTarStrip(r io.Reader, dest, prefix string, lim files.Limits) (*files.ExtractResult, error) {
	return s.extract(r, siteworker.Extract{Kind: siteworker.ExtractTar, Dest: dest, Prefix: prefix, Limits: lim})
}

func (s *siteFS) ExtractFile(r io.Reader, dest, name string, lim files.Limits) (*files.ExtractResult, error) {
	return s.extract(r, siteworker.Extract{Kind: siteworker.ExtractFile, Dest: dest, Name: name, Limits: lim})
}

// CountFiles counts the files under folder p.
func (s *siteFS) CountFiles(p string) (int, error) {
	var res siteworker.CountResult
	err := s.m.callSite(s.ctx, s.id, siteworker.MsgCount, siteworker.Count{Path: p}, &res, nil)
	return res.Files, err
}

// Symlink makes the link p to target, a relative path that stays in the
// site.
func (s *siteFS) Symlink(target, p string) error {
	return s.m.callSite(s.ctx, s.id, siteworker.MsgSymlink, siteworker.Symlink{Target: target, Path: p}, nil, nil)
}
