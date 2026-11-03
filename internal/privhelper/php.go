package privhelper

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/sysuser"
)

// SitePHP is what the agent tells of a site's PHP; the site's user, folder
// and sockets the helper derives from its id.
type SitePHP struct {
	SiteID   int64        `json:"site_id"`
	Version  string       `json:"version"`
	Settings php.Settings `json:"settings"`
	// DBSocket: by default the site's PHP reaches MariaDB on the socket the
	// agent serves for the site (a copy of a site, using its main server's
	// databases), not on the local server's.
	DBSocket bool `json:"db_socket,omitempty"`
}

// SiteLaravel is a site's PHP with the Laravel processes it should run.
type SiteLaravel struct {
	SitePHP
	Laravel php.Laravel `json:"laravel"`
}

// RestartedSites lists the sites whose PHP was restarted.
type RestartedSites struct {
	SiteIDs []int64 `json:"site_ids"`
}

// SiteDBSocket is the socket in dir on which the agent serves a site's
// database (helper and agent agree on the name).
func SiteDBSocket(dir string, siteID int64) string {
	return filepath.Join(dir, "site-"+strconv.FormatInt(siteID, 10)+".sock")
}

// pool is the site's PHP pool, every path and name from the helper's own
// configuration.
func (h *Helper) pool(s SitePHP) (php.Pool, error) {
	if s.SiteID <= 0 {
		return php.Pool{}, errSiteID
	}
	p := php.Pool{
		SiteID: s.SiteID, Version: s.Version, User: sysuser.SiteUser(s.SiteID),
		SocketGroup: h.cfg.WWWGroup, SiteDir: filepath.Join(h.cfg.SitesRoot, strconv.FormatInt(s.SiteID, 10)),
		Settings: s.Settings,
	}
	if s.DBSocket {
		p.DBSocket = SiteDBSocket(h.cfg.SiteDBDir, s.SiteID)
	}
	return p, nil
}

// ApplySitePHP installs (or replaces) the site's PHP pool, or its own
// service with the site's limits where each site's PHP runs as a systemd
// service.
func (h *Helper) ApplySitePHP(ctx context.Context, s SitePHP) error {
	p, err := h.pool(s)
	if err != nil {
		return err
	}
	h.phpMu.Lock()
	defer h.phpMu.Unlock()
	return h.php.Apply(ctx, p)
}

// RemoveSitePHP removes the site's PHP: pools, services, Laravel's
// processes.
func (h *Helper) RemoveSitePHP(_ context.Context, id int64) error {
	if id <= 0 {
		return errSiteID
	}
	h.phpMu.Lock()
	defer h.phpMu.Unlock()
	return h.php.Remove(id)
}

// ApplyLaravel makes the site's Laravel processes those of l.
func (h *Helper) ApplyLaravel(ctx context.Context, s SitePHP, l php.Laravel) error {
	p, err := h.pool(s)
	if err != nil {
		return err
	}
	h.phpMu.Lock()
	defer h.phpMu.Unlock()
	return h.php.ApplyLaravel(ctx, p, l)
}

// LaravelProcesses reports on, or restarts, the site's Laravel processes.
func (h *Helper) LaravelProcesses(ctx context.Context, req agentproto.SiteLaravel) (agentproto.SiteLaravelResponse, error) {
	var resp agentproto.SiteLaravelResponse
	if req.SiteID <= 0 {
		return resp, errSiteID
	}
	var err error
	switch req.Op {
	case agentproto.LaravelStatus:
		resp.Processes, err = h.php.LaravelStatus(ctx, req.SiteID)
	case agentproto.LaravelLog:
		resp.Lines, err = h.php.LaravelLog(ctx, req.SiteID, req.Lines)
	case agentproto.LaravelRestart:
		err = h.php.LaravelRestart(req.SiteID)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}
	return resp, err
}

// RestartUpdatedPHP restarts the sites' PHP services still running code
// an update replaced; it returns the sites restarted.
func (h *Helper) RestartUpdatedPHP(context.Context) ([]int64, error) {
	h.phpMu.Lock()
	defer h.phpMu.Unlock()
	ids, err := h.php.RestartUpdated()
	if ids == nil {
		ids = []int64{}
	}
	return ids, err
}

// MsgSiteDBSocket hands the socket of a site's database the agent serves
// (SiteDBSocket) to the site's user (SiteRequest): its PHP alone may use
// it.
const MsgSiteDBSocket = "site_db_socket"

// GiveSiteDBSocket makes the socket of the site's database the site
// user's, mode 0600. Only a socket of the agent's in the sockets' folder
// is changed, never through a link.
func (h *Helper) GiveSiteDBSocket(_ context.Context, id int64) error {
	if id <= 0 {
		return errSiteID
	}
	uid, _, err := sysuser.LookupUser(sysuser.SiteUser(id))
	if err != nil {
		return fmt.Errorf("site %d has no system user", id)
	}
	r, err := os.OpenRoot(h.cfg.SiteDBDir)
	if err != nil {
		return err
	}
	defer r.Close()
	name := filepath.Base(SiteDBSocket(h.cfg.SiteDBDir, id))
	fi, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s is not a socket", name)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != h.cfg.AgentUID && int(st.Uid) != uid {
		return fmt.Errorf("%s is not the agent's", name)
	}
	if err := r.Lchown(name, uid, -1); err != nil {
		return err
	}
	return r.Chmod(name, 0o600)
}
