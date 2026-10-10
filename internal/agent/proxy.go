package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/proxy"
)

const (
	fileProxyDesired  = "proxy-desired.json"
	proxyReconcileGap = 15 * time.Second
)

// proxyManager keeps the local proxy running the routes the panel wants.
// The desired state is persisted, so after a reboot sites come back even if
// the panel is unreachable.
type proxyManager struct {
	log       *slog.Logger
	stateFile string
	sitesRoot string
	opts      proxy.BuildOptions
	admin     *proxy.Admin
	sites     *sitesManager
	apps      *appsManager
	panel     *proxy.Route

	applyMu sync.Mutex // serialises Apply calls

	mu      sync.Mutex
	desired *proxy.Desired
	lastErr string
}

func newProxyManager(cfg Config, sites *sitesManager, apps *appsManager, log *slog.Logger) *proxyManager {
	var panel *proxy.Route
	if cfg.PanelDomain != "" && cfg.PanelUpstream != "" {
		panel = &proxy.Route{Kind: proxy.KindReverseProxy, Domains: []string{cfg.PanelDomain},
			Upstream: cfg.PanelUpstream, TLS: proxy.TLSAuto, HSTS: true}
	}
	return &proxyManager{
		panel:     panel,
		log:       log,
		sites:     sites,
		apps:      apps,
		stateFile: filepath.Join(cfg.DataDir, fileProxyDesired),
		sitesRoot: cfg.SitesRoot,
		opts: proxy.BuildOptions{
			Dir:           cfg.ProxyDir,
			HTTPPort:      cfg.HTTPPort,
			HTTPSPort:     cfg.HTTPSPort,
			ACMEDirectory: cfg.ACMEDirectory,
		},
		admin: proxy.NewAdmin(proxy.AdminSocket(cfg.ProxyDir)),
	}
}

func (m *proxyManager) loadSaved() error {
	b, err := os.ReadFile(m.stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d proxy.Desired
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("parse %s: %w", m.stateFile, err)
	}
	m.mu.Lock()
	m.desired = &d
	m.mu.Unlock()
	return nil
}

// Apply validates, persists and loads a new desired state. On error the
// proxy keeps serving the previous configuration.
func (m *proxyManager) Apply(ctx context.Context, d proxy.Desired) (string, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	cur := m.desired
	m.mu.Unlock()
	if cur != nil && d.Revision < cur.Revision {
		_, id, err := proxy.Build(m.withPanel(*cur), m.opts)
		m.log.Info("ignoring outdated proxy state", "revision", d.Revision, "current", cur.Revision)
		return id, err
	}

	m.resolve(&d)
	if err := d.Validate(m.sitesRoot); err != nil {
		return "", err
	}
	cfg, id, err := proxy.Build(m.withPanel(d), m.opts)
	if err != nil {
		return "", err
	}
	if err := m.admin.Load(ctx, cfg); err != nil {
		return "", err
	}
	// Persist only what the proxy accepted.
	if err := m.persist(d, cfg); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.desired = &d
	m.lastErr = ""
	m.mu.Unlock()
	m.log.Info("proxy configuration applied", "revision", d.Revision, "routes", len(d.Routes), "config_id", id)
	return id, nil
}

// withPanel adds the panel's own route. A site route claiming the panel's
// domain is dropped: losing access to the panel would be worse.
func (m *proxyManager) withPanel(d proxy.Desired) proxy.Desired {
	if m.panel == nil {
		return d
	}
	panel := *m.panel
	if err := panel.Validate(""); err != nil {
		m.log.Error("invalid panel route", "err", err)
		return d
	}
	out := d
	out.Routes = []proxy.Route{panel}
	for _, r := range d.Routes {
		if slices.ContainsFunc(r.Domains, func(x string) bool { return slices.Contains(panel.Domains, x) }) {
			m.log.Warn("route uses the panel's domain; skipped", "route_id", r.ID)
			continue
		}
		out.Routes = append(out.Routes, r)
	}
	return out
}

// resolve fills in what only the agent knows: the served directory of site
// routes. Values sent by the panel for these are ignored: only the agent
// decides where a site lives. App routes never carry an upstream.
func (m *proxyManager) resolve(d *proxy.Desired) {
	for i := range d.Routes {
		r := &d.Routes[i]
		switch {
		case r.Kind == proxy.KindStatic && r.SiteID > 0:
			r.Root = m.sites.PublicDir(r.SiteID)
			r.PHPSocket = ""
			if r.PHP {
				r.PHPSocket = m.sites.php.Socket(r.SiteID)
			}
		case r.Kind == proxy.KindReverseProxy && r.AppID > 0:
			r.Upstream = "" // looked up by the proxy, see pushAppUpstreams
		}
	}
}

func (m *proxyManager) persist(d proxy.Desired, cfg []byte) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.stateFile, b, 0o600); err != nil {
		return err
	}
	// The proxy loads this file on its own start. It may run as its own
	// user: hand the file to whoever owns the proxy directory.
	path := proxy.ConfigPath(m.opts.Dir)
	if err := writeAtomic(path, cfg, 0o600); err != nil {
		return err
	}
	return chownLike(path, m.opts.Dir)
}

// reconcile re-applies the desired state if the proxy is not running it,
// e.g. after the proxy process restarted or an app moved to a new container.
func (m *proxyManager) reconcile(ctx context.Context) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	var d proxy.Desired // nothing from the panel yet: still publish the panel
	have := m.desired != nil
	if have {
		d = *m.desired
		d.Routes = slices.Clone(d.Routes)
	}
	m.mu.Unlock()
	if !have && m.panel == nil {
		return
	}
	m.resolve(&d)
	cfg, id, err := proxy.Build(m.withPanel(d), m.opts)
	if err != nil {
		m.report(err)
		return
	}
	// Re-sent on every pass so a restarted proxy catches up; cheap.
	m.pushAppUpstreams(ctx)
	running, err := m.admin.Running(ctx, id)
	if err != nil {
		m.report(err)
		return
	}
	if running {
		return
	}
	if err := m.admin.Load(ctx, cfg); err != nil {
		m.report(err)
		return
	}
	m.log.Info("proxy configuration restored", "config_id", id)
}

// pushAppUpstreams sends the current container of every app to the proxy.
// This never reloads the proxy configuration, so deployments switch
// traffic without dropping connections.
func (m *proxyManager) pushAppUpstreams(ctx context.Context) {
	if m.apps == nil || m.apps.dc == nil {
		return
	}
	if err := m.admin.SetAppUpstreams(ctx, m.apps.Upstreams()); err != nil {
		m.report(err)
	}
}

// report logs an error once until it changes, so a stopped proxy does not
// flood the log every reconcile interval.
func (m *proxyManager) report(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err.Error() != m.lastErr {
		m.lastErr = err.Error()
		m.log.Warn("proxy reconcile", "err", err)
	}
}

func (m *proxyManager) run(ctx context.Context) {
	if err := m.loadSaved(); err != nil {
		m.log.Error("load saved proxy state", "err", err)
	}
	t := time.NewTicker(proxyReconcileGap)
	defer t.Stop()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
