package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/sysuser"
)

// On a server serving a copy of a site whose databases live on its
// primary server, the site's PHP reaches them through a socket of its own
// (<db dir>/site-<id>.sock, the default MariaDB socket of its PHP pool)
// that the agent carries through the tunnel to the primary's MariaDB.
// The routes are kept on disk so the sockets come back with the agent even
// while the panel is away.

const siteDBFile = "site-db.json"

type siteDBs struct {
	log     *slog.Logger
	dir     string // the sockets'
	state   string // the routes' file
	isolate bool
	tunnel  *tunnelManager

	mu     sync.Mutex
	routes map[int64]agentproto.SiteDBRoute
	ln     map[int64]net.Listener
	ctx    context.Context
}

func newSiteDBs(cfg Config, tunnel *tunnelManager, log *slog.Logger) *siteDBs {
	dir := cfg.SiteDBDir
	if dir == "" {
		dir = "/run/basalt/db"
	}
	return &siteDBs{log: log, dir: dir, state: filepath.Join(cfg.DataDir, siteDBFile), isolate: cfg.Isolate, tunnel: tunnel,
		routes: map[int64]agentproto.SiteDBRoute{}, ln: map[int64]net.Listener{}, ctx: context.Background()}
}

// Socket is the path of a site's database socket.
func (m *siteDBs) Socket(id int64) string {
	return filepath.Join(m.dir, "site-"+strconv.FormatInt(id, 10)+".sock")
}

// start opens the sockets of the routes kept on disk.
func (m *siteDBs) start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	b, err := os.ReadFile(m.state)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var routes map[int64]agentproto.SiteDBRoute
	if err == nil {
		err = json.Unmarshal(b, &routes)
	}
	if err != nil {
		m.log.Error("read site database routes", "err", err)
		return
	}
	for id, r := range routes {
		if _, err := m.set(id, &r); err != nil {
			m.log.Error("site database socket", "site_id", id, "err", err)
		}
	}
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, l := range m.ln {
			l.Close()
		}
	}()
}

// set gives a site a route to its databases (nil: none) and returns its
// socket ("" without a route).
func (m *siteDBs) set(id int64, r *agentproto.SiteDBRoute) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, had := m.routes[id]
	if r == nil {
		if !had {
			return "", nil
		}
		if l := m.ln[id]; l != nil {
			l.Close()
		}
		delete(m.ln, id)
		delete(m.routes, id)
		os.Remove(m.Socket(id))
		return "", m.save()
	}
	if r.ServerID <= 0 {
		return "", errors.New("bad database route")
	}
	if _, _, err := net.SplitHostPort(r.Addr); err != nil {
		return "", fmt.Errorf("bad database route: %w", err)
	}
	m.routes[id] = *r
	if had && old == *r && m.ln[id] != nil {
		return m.Socket(id), nil
	}
	if l := m.ln[id]; l != nil {
		l.Close()
	}
	l, err := m.listen(id)
	if err != nil {
		delete(m.routes, id)
		return "", err
	}
	m.ln[id] = l
	go m.serve(l, *r)
	return m.Socket(id), m.save()
}

// listen opens a site's socket, its user's alone.
func (m *siteDBs) listen(id int64) (net.Listener, error) {
	if err := os.MkdirAll(m.dir, 0o711); err != nil {
		return nil, err
	}
	path := m.Socket(id)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if m.isolate {
		uid, _, err := sysuser.LookupUser(sysuser.SiteUser(id))
		if err == nil {
			err = os.Chown(path, uid, -1)
		}
		if err == nil {
			err = os.Chmod(path, 0o600)
		}
		if err != nil {
			l.Close()
			return nil, err
		}
	} else if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (m *siteDBs) serve(l net.Listener, r agentproto.SiteDBRoute) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			m.mu.Lock()
			ctx := m.ctx
			m.mu.Unlock()
			remote, _, err := m.tunnel.dial(ctx, agentproto.PeerForward{ServerID: r.ServerID, Addr: r.Addr, Service: agentproto.ServiceMariaDB})
			if err != nil {
				m.log.Warn("site database through the tunnel", "server_id", r.ServerID, "err", err)
				return
			}
			defer remote.Close()
			splice(c, c, remote)
		}()
	}
}

func (m *siteDBs) save() error {
	b, _ := json.Marshal(m.routes)
	return writeAtomic(m.state, b, 0o600)
}
