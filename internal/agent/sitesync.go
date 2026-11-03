package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/siteworker"
)

// Copying a site's files between servers (the panel moves the data): the
// site's worker lists them, reads them on the primary and writes them on a
// replica (siteworker.Manifest, SyncRead, SyncWrite).

func (m *sitesManager) manifest(ctx context.Context, req agentproto.SiteManifestRequest, send func(any) error) (agentproto.SiteManifestResponse, error) {
	var resp agentproto.SiteManifestResponse
	if send == nil {
		return resp, errors.New("a manifest is a streaming request")
	}
	err := m.callSite(ctx, req.SiteID, siteworker.MsgManifest, req, &resp, func(f json.RawMessage) error {
		var frame agentproto.ManifestFrame
		if err := json.Unmarshal(f, &frame); err != nil {
			return err
		}
		return send(frame)
	})
	return resp, err
}

func (m *sitesManager) syncRead(ctx context.Context, req agentproto.SiteSyncReadRequest) (agentproto.SiteSyncReadResponse, error) {
	resp := agentproto.SiteSyncReadResponse{Chunks: []agentproto.SyncChunk{}}
	if err := req.Validate(); err != nil {
		return resp, err
	}
	err := m.callSite(ctx, req.SiteID, siteworker.MsgSyncRead, req, &resp, nil)
	return resp, err
}

func (m *sitesManager) syncWrite(ctx context.Context, req agentproto.SiteSyncWriteRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return m.callSite(ctx, req.SiteID, siteworker.MsgSyncWrite, req, nil, nil)
}
