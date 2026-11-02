// Package privhelper is the agent's privileged helper: "basalt
// agent-helper", a small root process that does for the agent the few
// things that need root, through a narrow typed interface (no "run a
// command" request, nor will there be one). The agent reaches it on a unix
// socket only the agent's user may use; requests and answers are
// agentproto frames.
//
// What it is told is only what it cannot know itself: a site's id, never a
// path, a user name or a group; those come from its own configuration.
// Every operation is idempotent, so the client may repeat one after the
// connection broke.
//
// Without a helper (development, servers installed before it existed) the
// agent runs the same Helper in its own process.
package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/wiefix/basalt/internal/sysuser"
)

// Command is the basalt subcommand of the helper.
const Command = "agent-helper"

// Messages the helper serves.
const (
	// MsgHostIdentity creates the web group and puts the proxy's user in it.
	MsgHostIdentity = "host_identity"
	// MsgSiteUserEnsure creates a site's system user (SiteUserRequest →
	// SiteUser); MsgSiteUserDelete removes it (SiteUserRequest).
	MsgSiteUserEnsure = "site_user_ensure"
	MsgSiteUserDelete = "site_user_delete"
)

type SiteUserRequest struct {
	SiteID int64 `json:"site_id"`
}

// SiteUser is a site's system user: its uid and the gid of its own group.
type SiteUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// Config is what the helper knows of the host by itself.
type Config struct {
	// SitesRoot holds the sites' folders (a site user's home is its folder).
	SitesRoot string
	// WWWGroup is shared by the site folders and the proxy (default
	// basalt-www); ProxyUser is the proxy's system user ("": none).
	WWWGroup  string
	ProxyUser string
}

// Helper carries out the privileged operations.
type Helper struct {
	cfg Config
	log *slog.Logger
	// identityMu: concurrent requests would race usermod against itself.
	identityMu sync.Mutex
}

func New(cfg Config, log *slog.Logger) (*Helper, error) {
	if cfg.WWWGroup == "" {
		cfg.WWWGroup = "basalt-www"
	}
	if !filepath.IsAbs(cfg.SitesRoot) {
		return nil, fmt.Errorf("sites root must be an absolute path, not %q", cfg.SitesRoot)
	}
	if err := sysuser.Check(cfg.WWWGroup); err != nil {
		return nil, err
	}
	return &Helper{cfg: cfg, log: log}, nil
}

// Handle serves one request of the agent (an agentproto.Handler).
func (h *Helper) Handle(ctx context.Context, msgType string, payload json.RawMessage) (any, error) {
	switch msgType {
	case MsgHostIdentity:
		return struct{}{}, h.HostIdentity(ctx)
	case MsgSiteUserEnsure, MsgSiteUserDelete:
		var req SiteUserRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		if msgType == MsgSiteUserDelete {
			return struct{}{}, h.DeleteSiteUser(ctx, req.SiteID)
		}
		uid, gid, err := h.EnsureSiteUser(ctx, req.SiteID)
		return SiteUser{UID: uid, GID: gid}, err
	}
	return nil, fmt.Errorf("unknown request %q", msgType)
}

func (h *Helper) HostIdentity(ctx context.Context) error {
	h.identityMu.Lock()
	defer h.identityMu.Unlock()
	if err := ensureGroup(ctx, h.cfg.WWWGroup); err != nil {
		return err
	}
	if h.cfg.ProxyUser == "" {
		return nil
	}
	return addToGroup(ctx, h.cfg.ProxyUser, h.cfg.WWWGroup)
}

var errSiteID = errors.New("invalid site id")

// EnsureSiteUser creates the system user of a site, whose home is the
// site's folder (which need not exist yet: a site restored from the trash
// gets its user first).
func (h *Helper) EnsureSiteUser(ctx context.Context, id int64) (uid, gid int, err error) {
	if id <= 0 {
		return -1, -1, errSiteID
	}
	name := sysuser.SiteUser(id)
	if uid, gid, err := sysuser.LookupUser(name); err == nil {
		return uid, gid, nil
	}
	uid, gid, err = ensureUser(ctx, name, filepath.Join(h.cfg.SitesRoot, strconv.FormatInt(id, 10)))
	if err == nil {
		h.log.Info("site user created", "site_id", id, "user", name, "uid", uid)
	}
	return uid, gid, err
}

func (h *Helper) DeleteSiteUser(ctx context.Context, id int64) error {
	if id <= 0 {
		return errSiteID
	}
	return deleteUser(ctx, sysuser.SiteUser(id))
}
