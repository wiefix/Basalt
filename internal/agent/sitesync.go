package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// Copying a site's files between servers (the panel moves the data): the
// manifest of a site, reads on the primary, writes on a replica. A site's
// logs/ and tmp/ belong to each server and are not copied.

// syncSkip leaves a site's own logs/ and tmp/ out of a copy.
func syncSkip(p string, dir bool) bool {
	return dir && (p == "logs" || p == "tmp")
}

// syncPath refuses paths a copy never touches.
func syncPath(p string) error {
	c, err := files.Clean(p)
	if err != nil {
		return err
	}
	if c == "." || c == "logs" || c == "tmp" || len(c) > 4 && (c[:5] == "logs/" || c[:4] == "tmp/") {
		return fmt.Errorf("%s is not copied between servers", p)
	}
	return nil
}

func (m *sitesManager) manifest(ctx context.Context, req agentproto.SiteManifestRequest, send func(any) error) (agentproto.SiteManifestResponse, error) {
	var resp agentproto.SiteManifestResponse
	if send == nil {
		return resp, errors.New("a manifest is a streaming request")
	}
	s, err := m.open(req.SiteID)
	if err != nil {
		return resp, err
	}
	defer s.Close()
	batch := make([]agentproto.ManifestEntry, 0, agentproto.ManifestBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := send(agentproto.ManifestFrame{Entries: batch})
		batch = batch[:0]
		return err
	}
	err = s.Walk(syncSkip, func(e files.WalkEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp.Count++
		if resp.Count > agentproto.MaxManifest {
			return fmt.Errorf("the site holds more than %d files and folders: too many to copy between servers", agentproto.MaxManifest)
		}
		me := agentproto.ManifestEntry{Path: e.Path, Dir: e.Dir, Mode: uint32(e.Mode)}
		if !e.Dir {
			me.Size, me.MTime = e.Size, e.MTime.UnixNano()
		}
		batch = append(batch, me)
		if len(batch) == agentproto.ManifestBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return resp, err
	}
	return resp, flush()
}

func (m *sitesManager) syncRead(req agentproto.SiteSyncReadRequest) (agentproto.SiteSyncReadResponse, error) {
	resp := agentproto.SiteSyncReadResponse{Chunks: []agentproto.SyncChunk{}}
	if err := req.Validate(); err != nil {
		return resp, err
	}
	s, err := m.open(req.SiteID)
	if err != nil {
		return resp, err
	}
	defer s.Close()
	for _, r := range req.Reads {
		if err := syncPath(r.Path); err != nil {
			return resp, err
		}
		data, size, mtime, missing, err := s.SyncRead(r.Path, r.Offset, r.Length)
		if err != nil {
			return resp, fmt.Errorf("read %s: %w", r.Path, err)
		}
		c := agentproto.SyncChunk{Path: r.Path, Offset: r.Offset, Data: data, Size: size, Missing: missing}
		if !missing {
			c.MTime = mtime.UnixNano()
		}
		resp.Chunks = append(resp.Chunks, c)
	}
	return resp, nil
}

func (m *sitesManager) syncWrite(req agentproto.SiteSyncWriteRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	s, err := m.open(req.SiteID)
	if err != nil {
		return err
	}
	defer s.Close()
	for _, p := range req.Delete {
		if err := syncPath(p); err != nil {
			return err
		}
		if err := s.Delete(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete %s: %w", p, err)
		}
	}
	for _, d := range req.Mkdir {
		if err := syncPath(d.Path); err != nil {
			return err
		}
		if err := s.SyncMkdir(d.Path, fs.FileMode(d.Mode)); err != nil {
			return err
		}
	}
	for _, w := range req.Write {
		if err := syncPath(w.Path); err != nil {
			return err
		}
		if err := s.SyncWrite(w.Path, w.Offset, w.Data, w.Final, fs.FileMode(w.Mode), time.Unix(0, w.MTime)); err != nil {
			return err
		}
	}
	for _, c := range req.Chmod {
		if err := syncPath(c.Path); err != nil {
			return err
		}
		if err := s.SyncChmod(c.Path, fs.FileMode(c.Mode)); err != nil {
			return fmt.Errorf("chmod %s: %w", c.Path, err)
		}
	}
	return nil
}
