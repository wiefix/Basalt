package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	trashDir       = ".trash"
)

var siteDirs = []string{"public", "logs", "tmp"}

const placeholderPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Site is ready</title></head>
<body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>It works!</h1><p>Upload your files to replace this page.</p></body></html>
`

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

	// workerExe is the binary run as a site's file worker (this one).
	workerExe string

	mu      sync.Mutex
	workers map[int64]*siteWorker
}

func newSitesManager(cfg Config, priv privileged, log *slog.Logger) *sitesManager {
	return &sitesManager{
		log: log, root: cfg.SitesRoot,
		isolate: cfg.Isolate, wwwGroup: cfg.WWWGroup,
		php:       php.DefaultHost(cfg.PHPRunDir),
		priv:      priv,
		workerExe: cfg.WorkerExe,
		workers:   map[int64]*siteWorker{},
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

	WriteMariaDBConfig(ctx context.Context, c privhelper.MariaDBConfig) error
	RestartMariaDB(ctx context.Context) error
}

func newPrivileged(cfg Config, log *slog.Logger) (privileged, error) {
	if cfg.HelperSocket != "" {
		return privhelper.NewClient(cfg.HelperSocket), nil
	}
	return privhelper.New(privhelper.Config{
		SitesRoot: cfg.SitesRoot, WWWGroup: cfg.WWWGroup, ProxyUser: cfg.ProxyUser,
		PHPRunDir: cfg.PHPRunDir, SiteDBDir: cfg.SiteDBDir, MariaDBConf: cfg.MariaDBConf,
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

	uid, gid, wwwGID := -1, -1, -1
	if m.isolate {
		if err := m.hostIdentity(ctx); err != nil {
			return fmt.Errorf("prepare host users: %w", err)
		}
		if uid, gid, err = m.priv.EnsureSiteUser(ctx, id); err != nil {
			return err
		}
		if wwwGID, err = sysuser.LookupGroup(m.wwwGroup); err != nil {
			return err
		}
	}

	// The site's folder is in the root-owned sites folder; what is inside
	// belongs to the site's user, who could swap a folder for a symlink
	// meanwhile: it is reached through os.Root and changed through the
	// open folder, never by path.
	if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	r, err := os.OpenRoot(d)
	if err != nil {
		return err
	}
	defer r.Close()
	layout := []struct {
		name string
		mode fs.FileMode
		gid  int
	}{
		{".", 0o750 | fs.ModeSetgid, wwwGID},
		{"public", 0o750 | fs.ModeSetgid, wwwGID},
		{"logs", 0o750 | fs.ModeSetgid, wwwGID},
		{"tmp", 0o700, gid},
	}
	for _, l := range layout {
		if l.name != "." {
			if err := r.Mkdir(l.name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		if !m.isolate {
			continue
		}
		if err := ownDir(r, l.name, uid, l.gid, l.mode); err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(d, l.name), err)
		}
	}

	s, err := m.open(id)
	if err != nil {
		return err
	}
	defer s.Close()
	if entries, err := s.List("/public"); err == nil && len(entries) == 0 {
		if err := s.WriteFile("/public/index.html", []byte(placeholderPage)); err != nil {
			return err
		}
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
	d, err := m.dir(id)
	if err != nil {
		return err
	}
	m.stopWorker(id)
	if err := m.priv.RemoveSitePHP(ctx, id); err != nil {
		m.log.Warn("remove PHP pool", "site_id", id, "err", err)
	}
	if _, err := os.Lstat(d); err == nil {
		trash := filepath.Join(m.root, trashDir)
		if err := os.MkdirAll(trash, 0o700); err != nil {
			return err
		}
		// The same site deleted twice within a second (a copy removed from
		// a server and the site deleted) takes the next free second.
		ts := time.Now().Unix()
		dest := filepath.Join(trash, fmt.Sprintf("%d-%d", id, ts))
		for _, err := os.Lstat(dest); err == nil; _, err = os.Lstat(dest) {
			ts++
			dest = filepath.Join(trash, fmt.Sprintf("%d-%d", id, ts))
		}
		if err := os.Rename(d, dest); err != nil {
			return err
		}
		m.log.Info("site moved to trash", "site_id", id, "path", dest)
	}
	if m.isolate {
		return m.priv.DeleteSiteUser(ctx, id)
	}
	return nil
}

// ownDir gives the folder name in r its owner and mode, through the open
// folder: a symlink put there instead cannot redirect the change (one
// swapped in after the check leads, at most, to another folder of the
// site).
func ownDir(r *os.Root, name string, uid, gid int, mode fs.FileMode) error {
	if fi, err := r.Lstat(name); err != nil || !fi.IsDir() {
		return errors.New("not a directory")
	}
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		return errors.New("not a directory")
	}
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// open gives file access to a site. With isolation, everything created is
// owned by the site's user so its PHP code can manage it.
func (m *sitesManager) open(id int64) (*files.Site, error) {
	d, err := m.dir(id)
	if err != nil {
		return nil, err
	}
	var s *files.Site
	if m.isolate {
		uid, _, lerr := sysuser.LookupUser(sysuser.SiteUser(id))
		if lerr != nil {
			return nil, fmt.Errorf("site %d has no system user", id)
		}
		s, err = files.OpenOwned(d, uid)
	} else {
		s, err = files.Open(d)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("site %d does not exist on this server", id)
	}
	return s, err
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

// removeTemps removes the sites' upload and copy temporaries last written
// before before.
func (m *sitesManager) removeTemps(ctx context.Context, before time.Time) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || id <= 0 || !e.IsDir() || ctx.Err() != nil {
			continue
		}
		s, err := files.Open(filepath.Join(m.root, e.Name()))
		if err != nil {
			continue
		}
		n, err := s.RemoveTemps(before)
		s.Close()
		if n > 0 || err != nil {
			m.log.Info("removed leftover temporary files", "site_id", id, "files", n, "err", err)
		}
	}
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
			m.cleanTrash(time.Now())
			if time.Since(swept) >= tempSweepEvery {
				swept = time.Now()
				m.removeTemps(ctx, swept.Add(-tempKeep))
			}
		}
		m.sweepWorkers(false)
	}
}
