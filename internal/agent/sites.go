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
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/sysuser"
)

const (
	uploadIdleTimeout = time.Hour
	// Temporaries of uploads and copies not written for tempKeep were left
	// behind (an upload idles out after uploadIdleTimeout); the janitor
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

	isolate   bool
	wwwGroup  string
	proxyUser string
	php       *php.Host

	identityOnce sync.Once
	identityErr  error

	mu      sync.Mutex
	uploads map[string]*pendingUpload
}

type pendingUpload struct {
	siteID   int64
	site     *files.Site
	upload   *files.Upload
	lastUsed time.Time
}

func newSitesManager(cfg Config, log *slog.Logger) *sitesManager {
	return &sitesManager{
		log: log, root: cfg.SitesRoot,
		isolate: cfg.Isolate, wwwGroup: cfg.WWWGroup, proxyUser: cfg.ProxyUser,
		php:     php.DefaultHost(cfg.PHPRunDir),
		uploads: map[string]*pendingUpload{},
	}
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

// PHPVersions lists the PHP-FPM versions installed on this host.
func (m *sitesManager) PHPVersions() []string { return m.php.Versions() }

// hostIdentity creates the shared web group and puts the proxy's user in it.
func (m *sitesManager) hostIdentity(ctx context.Context) error {
	m.identityOnce.Do(func() {
		if _, err := sysuser.EnsureGroup(ctx, m.wwwGroup); err != nil {
			m.identityErr = err
			return
		}
		if m.proxyUser != "" {
			m.identityErr = sysuser.AddToGroup(ctx, m.proxyUser, m.wwwGroup)
		}
	})
	return m.identityErr
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
		if uid, gid, err = sysuser.EnsureUser(ctx, sysuser.SiteUser(id), d); err != nil {
			return err
		}
		if wwwGID, err = sysuser.LookupGroup(m.wwwGroup); err != nil {
			return err
		}
	}

	layout := []struct {
		path string
		mode fs.FileMode
		gid  int
	}{
		{d, 0o750 | fs.ModeSetgid, wwwGID},
		{filepath.Join(d, "public"), 0o750 | fs.ModeSetgid, wwwGID},
		{filepath.Join(d, "logs"), 0o750 | fs.ModeSetgid, wwwGID},
		{filepath.Join(d, "tmp"), 0o700, gid},
	}
	for _, l := range layout {
		if err := os.Mkdir(l.path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if !m.isolate {
			continue
		}
		// Lchown/Chmod on paths we just created or validated as directories
		// under the root-owned sites directory.
		if fi, err := os.Lstat(l.path); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", l.path)
		}
		if err := os.Lchown(l.path, uid, l.gid); err != nil {
			return err
		}
		if err := os.Chmod(l.path, l.mode); err != nil {
			return err
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
		return m.php.Remove(id)
	}
	if !m.isolate {
		return errors.New("PHP sites need per-site user isolation (agent running as root on Linux)")
	}
	return m.php.Apply(ctx, php.Pool{
		SiteID: id, Version: phpVersion, User: sysuser.SiteUser(id),
		SocketGroup: m.wwwGroup, SiteDir: d, Settings: settings, DBSocket: dbSocket,
	})
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
	d, err := m.dir(id)
	if err != nil {
		return err
	}
	return m.php.ApplyLaravel(ctx, php.Pool{
		SiteID: id, Version: phpVersion, User: sysuser.SiteUser(id),
		SocketGroup: m.wwwGroup, SiteDir: d, Settings: settings, DBSocket: dbSocket,
	}, l)
}

// LaravelProcesses reports on, or restarts, the site's Laravel processes.
func (m *sitesManager) LaravelProcesses(ctx context.Context, req agentproto.SiteLaravel) (agentproto.SiteLaravelResponse, error) {
	var resp agentproto.SiteLaravelResponse
	if req.SiteID <= 0 {
		return resp, errors.New("bad site id")
	}
	var err error
	switch req.Op {
	case agentproto.LaravelStatus:
		resp.Processes, err = m.php.LaravelStatus(ctx, req.SiteID)
	case agentproto.LaravelLog:
		resp.Lines, err = m.php.LaravelLog(ctx, req.SiteID, req.Lines)
	case agentproto.LaravelRestart:
		err = m.php.LaravelRestart(req.SiteID)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}
	return resp, err
}

// Delete removes the site's PHP pool and system user and moves its files to
// the trash directory rather than erasing them, so an accidental deletion
// can be recovered by an operator.
func (m *sitesManager) Delete(ctx context.Context, id int64) error {
	d, err := m.dir(id)
	if err != nil {
		return err
	}
	m.abortUploads(id)
	if err := m.php.Remove(id); err != nil {
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
		return sysuser.DeleteUser(ctx, sysuser.SiteUser(id))
	}
	return nil
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
//
// Only a bare path error is rewritten: errors that already carry our own
// context (e.g. "php-fpm 8.4 is not running: ...") keep their message.
func publicError(err error) error {
	switch e := err.(type) {
	case *fs.PathError:
		return fmt.Errorf("%s: %w", e.Op, e.Err)
	case *os.LinkError:
		return fmt.Errorf("%s: %w", e.Op, e.Err)
	}
	return err
}

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
	out := msg
	for _, p := range h {
		out = p.re.ReplaceAllStringFunc(out, p.repl)
	}
	if out == msg {
		return err
	}
	return errors.New(out)
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m.php != nil {
			// After an update of PHP, sites must not keep running the old code.
			ids, err := m.php.RestartUpdated()
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
