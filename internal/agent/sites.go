package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/privhelper"
	"github.com/wiefix/basalt/internal/sysuser"
)

const (
	// Temporaries of uploads and copies not written for tempKeep were left
	// behind (an upload idles out after siteworker.UploadIdleTimeout); the janitor
	// looks for them every tempSweepEvery.
	tempKeep       = 6 * time.Hour
	tempSweepEvery = 6 * time.Hour
)

// sitesManager owns the per-site directories under SitesRoot:
//
//	<SitesRoot>/<id>/public   served by the proxy (and PHP-FPM)
//	<SitesRoot>/<id>/logs
//	<SitesRoot>/<id>/tmp
//
// With isolation on (Linux, agent running as root) each site has its own
// system user. Site directories are owned by that user with the shared
// web group, mode 2750: the proxy (a member of the web group) can read,
// other sites cannot even enter, and setgid hands the web group to new files.
type sitesManager struct {
	log  *slog.Logger
	root string

	isolate  bool
	wwwGroup string
	// php is read here (versions, sockets); the privileged helper changes
	// the sites' PHP.
	php *php.Host
	// priv creates and removes the site users (with isolation) and puts the
	// sites' PHP in place.
	priv privileged

	identityMu   sync.Mutex
	identityDone bool

	mu      sync.Mutex
	workers map[int64]*siteWorker
}

func newSitesManager(cfg Config, priv privileged, log *slog.Logger) *sitesManager {
	return &sitesManager{
		log: log, root: cfg.SitesRoot,
		isolate: cfg.Isolate, wwwGroup: cfg.WWWGroup,
		php:     php.DefaultHost(cfg.PHPRunDir),
		priv:    priv,
		workers: map[int64]*siteWorker{},
	}
}

func (m *sitesManager) dir(id int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("invalid site id %d", id)
	}
	return filepath.Join(m.root, strconv.FormatInt(id, 10)), nil
}

// privileged is what the agent has the privileged helper do: a
// privhelper.Client, or a privhelper.Helper in this process when the host
// has no helper (development, servers installed before it).
type privileged interface {
	HostIdentity(ctx context.Context) error
	EnsureSiteUser(ctx context.Context, id int64) (uid, gid int, err error)
	DeleteSiteUser(ctx context.Context, id int64) error

	ApplySitePHP(ctx context.Context, s privhelper.SitePHP) error
	RemoveSitePHP(ctx context.Context, id int64) error
	ApplyLaravel(ctx context.Context, s privhelper.SitePHP, l php.Laravel) error
	LaravelProcesses(ctx context.Context, req agentproto.SiteLaravel) (agentproto.SiteLaravelResponse, error)
	RestartUpdatedPHP(ctx context.Context) ([]int64, error)

	EnsureSiteFolder(ctx context.Context, id int64) error
	TrashSiteFolder(ctx context.Context, id int64) error
	Trash(ctx context.Context, req agentproto.SiteTrash) (agentproto.SiteTrashResponse, error)
	CleanTrash(ctx context.Context) error
	SiteUsage(ctx context.Context, ids []int64) (privhelper.SiteUsageResult, error)
	RemoveTemps(ctx context.Context, before time.Time) (privhelper.RemovedTemps, error)
	SiteProcess(ctx context.Context, req privhelper.SiteProcess, stdio, stderr *os.File) (privhelper.SiteProcessResult, error)

	WriteMariaDBConfig(ctx context.Context, c privhelper.MariaDBConfig) error
	RestartMariaDB(ctx context.Context) error
	MariaDBAccount(ctx context.Context) error
	GiveSiteDBSocket(ctx context.Context, id int64) error

	Restic(ctx context.Context, req privhelper.ResticRequest, stdio privhelper.Stdio, line func(string)) (privhelper.ResticResult, error)

	Updatable(ctx context.Context) (bool, error)
	StageUpdate(ctx context.Context, req agentproto.UpdatePrepare, bin *os.File) error
	ActivateUpdate(ctx context.Context, req agentproto.UpdateActivate) error

	BuildKit(ctx context.Context, namespace string) (string, error)

	SetFloatingIPs(ctx context.Context, f agentproto.FloatingIPs) error
}

func newPrivileged(cfg Config, log *slog.Logger) (privileged, error) {
	if cfg.HelperSocket != "" {
		return privhelper.NewClient(cfg.HelperSocket), nil
	}
	return privhelper.New(privhelper.Config{
		SitesRoot: cfg.SitesRoot, WWWGroup: cfg.WWWGroup, ProxyUser: cfg.ProxyUser,
		PHPRunDir: cfg.PHPRunDir, SiteDBDir: cfg.SiteDBDir, MariaDBConf: cfg.MariaDBConf,
		Restic: cfg.Restic, StateDir: cfg.DataDir, AgentDataDir: cfg.DataDir,
		ReleaseKey: cfg.ReleaseKey, UpdateExe: cfg.UpdateExe, Restart: cfg.Restart,
		DockerSocket: cmp.Or(cfg.DockerUpstream, cfg.DockerSocket),
		Isolate:      cfg.Isolate, WorkerExe: cfg.WorkerExe, AgentUID: os.Geteuid(),
	}, log)
}

// PublicDir is where the proxy serves a site from.
func (m *sitesManager) PublicDir(id int64) string {
	d, _ := m.dir(id)
	return filepath.Join(d, "public")
}

// PHPVersions lists the PHP-FPM versions installed on this host.
func (m *sitesManager) PHPVersions() []string { return m.php.Versions() }

// hostIdentity has the shared web group created and the proxy's user put
// in it, once (a failure, say the helper not running yet, is retried).
func (m *sitesManager) hostIdentity(ctx context.Context) error {
	m.identityMu.Lock()
	defer m.identityMu.Unlock()
	if m.identityDone {
		return nil
	}
	if err := m.priv.HostIdentity(ctx); err != nil {
		return err
	}
	m.identityDone = true
	return nil
}

// Ensure creates the site's directories, system user and PHP pool
// (idempotent). A new, empty public directory gets a placeholder page.
// hasUser reports whether the site's system user exists (always, without
// isolation).
func (m *sitesManager) hasUser(id int64) bool {
	if !m.isolate {
		return true
	}
	_, _, err := sysuser.LookupUser(sysuser.SiteUser(id))
	return err == nil
}

// Ensure makes the site exist: user, folders, PHP pool (with dbSocket, if
// set, as its default MariaDB socket).
func (m *sitesManager) Ensure(ctx context.Context, id int64, phpVersion string, settings php.Settings, dbSocket string) error {
	if _, err := m.dir(id); err != nil {
		return err
	}
	if m.isolate {
		if err := m.hostIdentity(ctx); err != nil {
			return fmt.Errorf("prepare host users: %w", err)
		}
	}
	// The helper makes the folders (and the site's user, who owns them).
	if err := m.priv.EnsureSiteFolder(ctx, id); err != nil {
		return err
	}

	if phpVersion == "" {
		return m.priv.RemoveSitePHP(ctx, id)
	}
	if !m.isolate {
		return errors.New("PHP sites need per-site user isolation (agent running as root on Linux)")
	}
	// The pools' sockets go to the agent's runtime folder, which the
	// helper cannot write to.
	if err := os.MkdirAll(m.php.RunDir, 0o755); err != nil {
		return err
	}
	return m.priv.ApplySitePHP(ctx, sitePHP(id, phpVersion, settings, dbSocket))
}

// sitePHP is the site's PHP as the helper is told it: a dbSocket (the
// agent's socket for the site's database) is one the helper knows the
// path of.
func sitePHP(id int64, version string, settings php.Settings, dbSocket string) privhelper.SitePHP {
	return privhelper.SitePHP{SiteID: id, Version: version, Settings: settings, DBSocket: dbSocket != ""}
}

// Laravel makes the site's Laravel processes (scheduler, queue workers)
// those of l; a static site has none (Ensure removed them).
func (m *sitesManager) Laravel(ctx context.Context, id int64, phpVersion string, settings php.Settings, dbSocket string, l php.Laravel) error {
	if phpVersion == "" {
		if !l.IsZero() {
			return errors.New("Laravel processes need a PHP site")
		}
		return nil
	}
	if id <= 0 {
		return fmt.Errorf("invalid site id %d", id)
	}
	return m.priv.ApplyLaravel(ctx, sitePHP(id, phpVersion, settings, dbSocket), l)
}

// LaravelProcesses reports on, or restarts, the site's Laravel processes.
func (m *sitesManager) LaravelProcesses(ctx context.Context, req agentproto.SiteLaravel) (agentproto.SiteLaravelResponse, error) {
	if req.SiteID <= 0 {
		return agentproto.SiteLaravelResponse{}, errors.New("bad site id")
	}
	return m.priv.LaravelProcesses(ctx, req)
}

// Delete removes the site's PHP pool and system user and moves its files to
// the trash directory rather than erasing them, so an accidental deletion
// can be recovered by an operator.
func (m *sitesManager) Delete(ctx context.Context, id int64) error {
	if _, err := m.dir(id); err != nil {
		return err
	}
	m.stopWorker(id)
	if err := m.priv.RemoveSitePHP(ctx, id); err != nil {
		m.log.Warn("remove PHP pool", "site_id", id, "err", err)
	}
	if err := m.priv.TrashSiteFolder(ctx, id); err != nil {
		return err
	}
	if m.isolate {
		return m.priv.DeleteSiteUser(ctx, id)
	}
	return nil
}

// publicError strips absolute host paths from errors before they leave the
// agent; the panel only needs to see paths relative to the site.
//
// Only a bare path error is rewritten: errors that already carry our own
// context (e.g. "php-fpm 8.4 is not running: ...") keep their message.
func publicError(err error) error { return files.PublicError(err) }

// hostPaths rewrites the agent's own directories in every error that
// leaves the agent, so a path missed by publicError (one wrapped with
// context, say) still does not reach the panel: a site's directory
// becomes "<site>", the others a name for the directory.
type hostPaths []hostPath

type hostPath struct {
	re   *regexp.Regexp
	repl func(match string) string
}

func newHostPaths(cfg Config) hostPaths {
	type dir struct {
		path  string
		sites bool
		name  string
	}
	dirs := []dir{{cfg.SitesRoot, true, "<sites>"}, {cfg.DataDir, false, "<agent-data>"}, {cfg.ProxyDir, false, "<proxy-data>"}}
	if cfg.HelperSocket != "" && cfg.DataDir != "" {
		// The helper's own data (restic's cache, SSH keys of storages),
		// next to the agent's as the installer sets them up.
		dirs = append(dirs, dir{filepath.Join(filepath.Dir(cfg.DataDir), "helper"), false, "<helper-data>"})
	}
	// Nested directories first, so the longest prefix names the path.
	slices.SortFunc(dirs, func(a, b dir) int { return len(b.path) - len(a.path) })
	var h hostPaths
	for _, d := range dirs {
		if !filepath.IsAbs(d.path) {
			continue
		}
		root := regexp.QuoteMeta(filepath.Clean(d.path))
		if d.sites {
			re := regexp.MustCompile(root + `(/[0-9]+)?\b`)
			h = append(h, hostPath{re, func(m string) string {
				if re.FindStringSubmatch(m)[1] != "" {
					return "<site>"
				}
				return "<sites>"
			}})
			continue
		}
		name := d.name
		h = append(h, hostPath{regexp.MustCompile(root + `\b`), func(string) string { return name }})
	}
	return h
}

// scrub returns err unchanged unless its message names a host directory.
func (h hostPaths) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if out := h.text(msg); out != msg {
		return errors.New(out)
	}
	return err
}

// text rewrites the host directories in s (log output leaving the agent).
func (h hostPaths) text(s string) string {
	for _, p := range h {
		s = p.re.ReplaceAllStringFunc(s, p.repl)
	}
	return s
}

// janitor aborts uploads abandoned by the panel (e.g. browser closed),
// cleans up, and has the sites' PHP run its current code. Every minute: a
// security update of PHP must reach the sites soon.
func (m *sitesManager) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var swept time.Time
	defer m.sweepWorkers(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m.priv != nil {
			// After an update of PHP, sites must not keep running the old code.
			ids, err := m.priv.RestartUpdatedPHP(ctx)
			if len(ids) > 0 {
				m.log.Info("restarted the PHP of sites to run the updated PHP", "sites", ids)
			}
			if err != nil {
				m.log.Warn("restart the PHP of sites after an update", "err", err)
			}
		}
		if m.root != "" {
			if err := m.priv.CleanTrash(ctx); err != nil {
				m.log.Warn("clean the trash", "err", err)
			}
			if time.Since(swept) >= tempSweepEvery {
				swept = time.Now()
				if _, err := m.priv.RemoveTemps(ctx, swept.Add(-tempKeep)); err != nil {
					m.log.Warn("remove leftover temporary files", "err", err)
				}
			}
		}
		m.sweepWorkers(false)
	}
}
