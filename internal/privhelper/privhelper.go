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
// It creates and removes the sites' users and folders (and the trash of
// deleted sites), starts the sites' processes as their users (file
// workers, SFTP sessions, PHP's CLI), puts the sites' PHP in place (pools,
// their systemd services, Laravel's processes), keeps MariaDB's option
// file of database replication and gives the agent's user its MariaDB
// account, puts the floating IPs on the host's interface, runs restic for backups (it reads and restores every site's
// files), installs updates of Basalt, and stands between the agent and
// Docker (ServeDocker, BuildKit). The agent runs as a user of its own
// (basalt-agent); MariaDB's databases it manages with its account.
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
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/sshsig"
	"github.com/wiefix/basalt/internal/sysuser"
)

// Command is the basalt subcommand of the helper.
const Command = "agent-helper"

// Messages the helper serves.
const (
	// MsgHostIdentity creates the web group and puts the proxy's user in it.
	MsgHostIdentity = "host_identity"
	// MsgSiteUserEnsure creates a site's system user (SiteRequest →
	// SiteUser); MsgSiteUserDelete removes it (SiteRequest).
	MsgSiteUserEnsure = "site_user_ensure"
	MsgSiteUserDelete = "site_user_delete"

	// MsgSitePHPApply puts a site's PHP in place: its pool or its own FPM
	// service (SitePHP); MsgSitePHPRemove takes it away (SiteRequest).
	MsgSitePHPApply  = "site_php_apply"
	MsgSitePHPRemove = "site_php_remove"
	// MsgSiteLaravelApply sets a site's Laravel processes (SiteLaravel);
	// MsgSiteLaravel reports on or restarts them (agentproto.SiteLaravel →
	// agentproto.SiteLaravelResponse).
	MsgSiteLaravelApply = "site_laravel_apply"
	MsgSiteLaravel      = "site_laravel"
	// MsgPHPRestartUpdated restarts the sites' PHP that runs code replaced
	// by an update (→ RestartedSites).
	MsgPHPRestartUpdated = "php_restart_updated"

	// MsgMariaDBConfig writes MariaDB's option file of database
	// replication (MariaDBConfig); MsgMariaDBRestart restarts MariaDB.
	MsgMariaDBConfig  = "mariadb_config"
	MsgMariaDBRestart = "mariadb_restart"
	// MsgMariaDBAccount gives the agent's user its account of MariaDB.
	MsgMariaDBAccount = "mariadb_account"
)

// SiteRequest names a site.
type SiteRequest struct {
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
	// PHPRunDir holds the sites' PHP-FPM sockets (default /run/basalt/php);
	// SiteDBDir the sockets of the sites' databases the agent serves
	// (default /run/basalt/db).
	PHPRunDir string
	SiteDBDir string
	// MariaDBConf is the option file of database replication (default
	// /etc/mysql/mariadb.conf.d/90-basalt.cnf); MySQLSocket MariaDB's
	// socket (default /run/mysqld/mysqld.sock).
	MariaDBConf string
	MySQLSocket string
	// AgentUser is the agent's system user ("" or root: the agent is
	// root and needs no account of its own), AgentUID its uid.
	AgentUser string
	AgentUID  int
	// PHP is the host's PHP (default: php.DefaultHost of PHPRunDir).
	PHP *php.Host

	// Restic is the restic binary backups run with ("" disables them).
	// StateDir keeps restic's cache and the SSH keys of SFTP storages
	// (default /var/lib/basalt/helper); AgentDataDir is the agent's data,
	// where no local repository may be.
	Restic       string
	StateDir     string
	AgentDataDir string

	// ReleaseKey is the key release checksums must be signed with (default:
	// the Basalt release key). UpdateExe is the binary an update replaces
	// (default: this one, if the installer set the host up; "": no
	// updates) and Restart restarts the services (default: systemctl).
	ReleaseKey string
	UpdateExe  string
	Restart    func(units []string) error

	// DockerSocket is Docker's own socket ("": no Docker): the helper
	// serves its API to the agent through dockerproxy and runs the
	// BuildKit container.
	DockerSocket string

	// Isolate: each site has its own system user, whose rights the site's
	// processes and files get (the helper is root on Linux). Without it
	// (development) they run as, and belong to, this process's user.
	Isolate bool
	// WorkerExe is the binary of the sites' file workers (default: this
	// one); PHPBinary the PHP CLI of a version (default /usr/bin/php<v>).
	WorkerExe string
	PHPBinary func(version string) string
}

// Helper carries out the privileged operations.
type Helper struct {
	cfg Config
	log *slog.Logger
	php *php.Host
	// identityMu: concurrent requests would race usermod against itself.
	identityMu sync.Mutex
	// phpMu serializes changes of the sites' PHP: they rewrite shared
	// files (pools of a version, systemd's units) and reload.
	phpMu sync.Mutex
	// mariadbMu: one writer of the option file at a time.
	mariadbMu sync.Mutex
	// updateMu guards staged, the version waiting in UpdateExe + ".new".
	updateMu sync.Mutex
	staged   string
	// buildKitMu: one check of the BuildKit container at a time.
	buildKitMu sync.Mutex
	// floatingMu: one change of the floating IPs at a time; ip replaces
	// iproute2 in tests.
	floatingMu sync.Mutex
	ip         func(ctx context.Context, args ...string) ([]byte, error)
}

func New(cfg Config, log *slog.Logger) (*Helper, error) {
	if cfg.WWWGroup == "" {
		cfg.WWWGroup = "basalt-www"
	}
	if cfg.PHPRunDir == "" {
		cfg.PHPRunDir = "/run/basalt/php"
	}
	if cfg.SiteDBDir == "" {
		cfg.SiteDBDir = "/run/basalt/db"
	}
	if cfg.MariaDBConf == "" {
		cfg.MariaDBConf = DefaultMariaDBConf
	}
	if cfg.MySQLSocket == "" {
		cfg.MySQLSocket = "/run/mysqld/mysqld.sock"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/basalt/helper"
	}
	if cfg.ReleaseKey == "" {
		cfg.ReleaseKey = sshsig.ReleaseKey
	}
	if cfg.UpdateExe == "" {
		cfg.UpdateExe = updateExe()
	}
	if cfg.Restart == nil {
		cfg.Restart = systemctlRestart
	}
	if cfg.WorkerExe == "" {
		// Resolved now: after an update replaces the binary, the path
		// leads to the new one (the old one shows as "(deleted)").
		cfg.WorkerExe, _ = os.Executable()
	}
	if cfg.PHPBinary == nil {
		cfg.PHPBinary = func(v string) string { return "/usr/bin/php" + v }
	}
	for _, p := range []string{cfg.PHPRunDir, cfg.SiteDBDir, cfg.MariaDBConf, cfg.StateDir} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%q must be an absolute path", p)
		}
	}
	if !filepath.IsAbs(cfg.SitesRoot) {
		return nil, fmt.Errorf("sites root must be an absolute path, not %q", cfg.SitesRoot)
	}
	if err := sysuser.Check(cfg.WWWGroup); err != nil {
		return nil, err
	}
	h := &Helper{cfg: cfg, log: log, php: cfg.PHP}
	if h.php == nil {
		h.php = php.DefaultHost(cfg.PHPRunDir)
	}
	return h, nil
}

// decode reads a request's payload.
func decode[T any](payload json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(payload, &v)
	return v, err
}

// Handle serves one request of the agent (an agentproto.Handler).
func (h *Helper) Handle(ctx context.Context, msgType string, payload json.RawMessage) (any, error) {
	switch msgType {
	case MsgHostIdentity:
		return struct{}{}, h.HostIdentity(ctx)
	case MsgSiteUserEnsure, MsgSiteUserDelete:
		req, err := decode[SiteRequest](payload)
		if err != nil {
			return nil, err
		}
		if msgType == MsgSiteUserDelete {
			return struct{}{}, h.DeleteSiteUser(ctx, req.SiteID)
		}
		uid, gid, err := h.EnsureSiteUser(ctx, req.SiteID)
		return SiteUser{UID: uid, GID: gid}, err
	case MsgSitePHPApply:
		req, err := decode[SitePHP](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.ApplySitePHP(ctx, req)
	case MsgSitePHPRemove:
		req, err := decode[SiteRequest](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.RemoveSitePHP(ctx, req.SiteID)
	case MsgSiteLaravelApply:
		req, err := decode[SiteLaravel](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.ApplyLaravel(ctx, req.SitePHP, req.Laravel)
	case MsgSiteLaravel:
		req, err := decode[agentproto.SiteLaravel](payload)
		if err != nil {
			return nil, err
		}
		return h.LaravelProcesses(ctx, req)
	case MsgPHPRestartUpdated:
		ids, err := h.RestartUpdatedPHP(ctx)
		return RestartedSites{SiteIDs: ids}, err
	case MsgMariaDBConfig:
		req, err := decode[MariaDBConfig](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.WriteMariaDBConfig(ctx, req)
	case MsgMariaDBRestart:
		return struct{}{}, h.RestartMariaDB(ctx)
	case MsgSiteDBSocket:
		req, err := decode[SiteRequest](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.GiveSiteDBSocket(ctx, req.SiteID)
	case MsgMariaDBAccount:
		return struct{}{}, h.MariaDBAccount(ctx)
	case MsgRestic:
		return h.handleRestic(ctx, payload)
	case MsgBuildKit:
		req, err := decode[BuildKitRequest](payload)
		if err != nil {
			return nil, err
		}
		name, err := h.BuildKit(ctx, req.Namespace)
		return BuildKitContainer{Name: name}, err
	case MsgSiteFolderEnsure, MsgSiteFolderTrash:
		req, err := decode[SiteRequest](payload)
		if err != nil {
			return nil, err
		}
		if msgType == MsgSiteFolderTrash {
			return struct{}{}, h.TrashSiteFolder(ctx, req.SiteID)
		}
		return struct{}{}, h.EnsureSiteFolder(ctx, req.SiteID)
	case MsgTrash:
		req, err := decode[agentproto.SiteTrash](payload)
		if err != nil {
			return nil, err
		}
		return h.Trash(ctx, req)
	case MsgTrashClean:
		return struct{}{}, h.CleanTrash(ctx)
	case MsgSiteUsage:
		req, err := decode[SiteUsageRequest](payload)
		if err != nil {
			return nil, err
		}
		return h.SiteUsage(ctx, req.SiteIDs)
	case MsgSiteTemps:
		req, err := decode[SiteTemps](payload)
		if err != nil {
			return nil, err
		}
		return h.RemoveTemps(ctx, req.Before)
	case MsgSiteProcess:
		req, err := decode[SiteProcess](payload)
		if err != nil {
			return nil, err
		}
		files := requestFiles(ctx)
		var stdio, stderr *os.File
		if len(files) > 0 {
			stdio = files[0]
		}
		if len(files) > 1 {
			stderr = files[1]
		}
		return h.SiteProcess(ctx, req, stdio, stderr)
	case MsgFloatingIPs:
		req, err := decode[agentproto.FloatingIPs](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.SetFloatingIPs(ctx, req)
	case MsgUpdateInfo, MsgUpdateStage, MsgUpdateActivate:
		return h.handleUpdate(ctx, msgType, payload)
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
