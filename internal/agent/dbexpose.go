package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

const fileDBExpose = "db-expose.json"

// Limits on the connections to one public database.
const (
	dbExposeMaxConns      = 256
	dbExposeMaxConnsPerIP = 32
	dbExposeDialTimeout   = 5 * time.Second
	// dbExposeTargetTTL is how long the address of a database's container
	// is reused before Docker is asked again; a failed dial asks at once.
	dbExposeTargetTTL = 5 * time.Second
)

// dbTarget finds where a database's container listens: the agent dials
// it for every connection it forwards.
type dbTarget func(ctx context.Context, appID int64, service string, port int) (string, error)

// dbExposeManager makes container databases reachable from outside the
// host (agentproto.TypeDBExpose): it listens on each public port and
// forwards connections from the allowed addresses to the container. The
// entries are kept on disk, so access survives restarts while the panel
// is down.
type dbExposeManager struct {
	log       *slog.Logger
	stateFile string
	target    dbTarget

	mu        sync.Mutex
	listeners map[int64]*dbListener // by app
	errs      map[int64]string      // why an entry does not listen
	ctx       context.Context       // set by start; listeners stop with it

	cert atomic.Pointer[tls.Certificate] // presented to clients; nil: no TLS
}

func newDBExposeManager(cfg Config, target dbTarget, log *slog.Logger) *dbExposeManager {
	return &dbExposeManager{
		log: log, stateFile: filepath.Join(cfg.DataDir, fileDBExpose), target: target,
		listeners: map[int64]*dbListener{}, errs: map[int64]string{},
	}
}

// start serves the saved entries until ctx is done.
func (m *dbExposeManager) start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	b, err := os.ReadFile(m.stateFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Error("load public databases", "err", err)
	}
	if err == nil {
		var req agentproto.DBExposeApply
		if err := json.Unmarshal(b, &req); err != nil {
			m.log.Error("load public databases", "err", fmt.Errorf("parse %s: %w", m.stateFile, err))
		} else if err := req.Validate(); err != nil {
			m.log.Error("load public databases", "err", err)
		} else {
			m.apply(req)
		}
	}
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		for id, l := range m.listeners {
			l.close()
			delete(m.listeners, id)
		}
	}()
}

// Apply stores the complete list of public databases and serves it.
func (m *dbExposeManager) Apply(req agentproto.DBExposeApply) (agentproto.DBExposeResponse, error) {
	if err := req.Validate(); err != nil {
		return agentproto.DBExposeResponse{}, err
	}
	if _, err := parseDBCert(req.TLS); err != nil {
		return agentproto.DBExposeResponse{}, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return agentproto.DBExposeResponse{}, err
	}
	if err := writeAtomic(m.stateFile, b, 0o600); err != nil {
		return agentproto.DBExposeResponse{}, publicError(err)
	}
	return m.apply(req), nil
}

// parseDBCert reads the certificate public databases present.
func parseDBCert(t *agentproto.DBExposeTLS) (*tls.Certificate, error) {
	if t == nil {
		return nil, nil
	}
	c, err := tls.X509KeyPair([]byte(t.Cert), []byte(t.Key))
	if err != nil {
		return nil, fmt.Errorf("public database certificate: %w", err)
	}
	return &c, nil
}

func (m *dbExposeManager) apply(req agentproto.DBExposeApply) agentproto.DBExposeResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	// New connections take the new certificate; open ones keep theirs.
	cert, err := parseDBCert(req.TLS)
	if err != nil { // only from a damaged state file: Apply checked it
		m.log.Error("load public databases", "err", err)
	}
	m.cert.Store(cert)
	want := map[int64]agentproto.DBExpose{}
	for _, e := range req.Entries {
		want[e.AppID] = e
	}
	// Listeners that go or change their port close first, so a port
	// moving from one database to another is free again.
	for id, l := range m.listeners {
		if e, ok := want[id]; !ok || e.Port != l.entry.Port {
			l.close()
			delete(m.listeners, id)
		}
	}
	m.errs = map[int64]string{}
	resp := agentproto.DBExposeResponse{Entries: []agentproto.DBExposeStatus{}}
	for _, e := range req.Entries {
		allow, _ := agentproto.ParseAllow(e.Allow) // validated
		if l, ok := m.listeners[e.AppID]; ok {
			l.update(e, allow)
		} else if m.ctx != nil {
			l, err := m.listen(e, allow)
			if err != nil {
				m.log.Warn("public database cannot listen", "app_id", e.AppID, "port", e.Port, "err", err)
				m.errs[e.AppID] = publicError(err).Error()
			} else {
				m.listeners[e.AppID] = l
			}
		}
		resp.Entries = append(resp.Entries, agentproto.DBExposeStatus{AppID: e.AppID, Error: m.errs[e.AppID]})
	}
	if len(req.Entries) > 0 || len(m.listeners) > 0 {
		m.log.Info("public databases applied", "entries", len(req.Entries), "listening", len(m.listeners))
	}
	return resp
}

func (m *dbExposeManager) listen(e agentproto.DBExpose, allow []netip.Prefix) (*dbListener, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(e.Port))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", e.Port, err)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	l := &dbListener{
		log: m.log.With("app_id", e.AppID, "port", e.Port), ln: ln, target: m.target, cert: &m.cert, ctx: ctx, cancel: cancel,
		entry: e, allow: allow, conns: map[net.Conn]netip.Addr{}, perIP: map[netip.Addr]int{},
	}
	go l.serve()
	return l, nil
}

// dbListener serves one public database.
type dbListener struct {
	log    *slog.Logger
	ln     net.Listener
	target dbTarget
	cert   *atomic.Pointer[tls.Certificate]
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	entry    agentproto.DBExpose
	allow    []netip.Prefix
	conns    map[net.Conn]netip.Addr // client connections being forwarded
	perIP    map[netip.Addr]int
	addr     string // the container's address, until addrTill
	addrTill time.Time
}

func allowed(allow []netip.Prefix, a netip.Addr) bool {
	return slices.ContainsFunc(allow, func(p netip.Prefix) bool { return p.Contains(a) })
}

// update takes a new allow list (and container port); connections from
// addresses no longer allowed are closed.
func (l *dbListener) update(e agentproto.DBExpose, allow []netip.Prefix) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Service != l.entry.Service || e.TargetPort != l.entry.TargetPort {
		l.addr = ""
	}
	l.entry, l.allow = e, allow
	for c, a := range l.conns {
		if !allowed(allow, a) {
			c.Close()
		}
	}
}

func (l *dbListener) close() {
	l.cancel()
	l.ln.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		c.Close()
	}
}

func (l *dbListener) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if l.ctx.Err() == nil {
				l.log.Warn("public database stopped accepting", "err", err)
			}
			return
		}
		go l.handle(c)
	}
}

func (l *dbListener) handle(c net.Conn) {
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		c.Close()
		return
	}
	ip := ap.Addr().Unmap()
	if !l.admit(c, ip) {
		c.Close()
		return
	}
	defer l.release(c, ip)
	l.mu.Lock()
	e := l.entry
	l.mu.Unlock()
	cc, up, err := negotiate(e.Protocol, dbTLS{cert: l.cert.Load(), require: e.RequireTLS}, c, l.dial)
	if err != nil {
		switch {
		case errors.Is(err, errDial):
			l.log.Warn("public database: cannot reach the container", "err", err)
		case !errors.Is(err, errTLSRequired):
			l.log.Debug("public database: connection start failed", "ip", ip, "err", err)
		}
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(up, cc)
	go pipe(cc, up)
	// Either side closing ends the connection (database protocols do not
	// half-close).
	select {
	case <-done:
	case <-l.ctx.Done():
	}
}

// admit registers a connection from an allowed address within the limits.
func (l *dbListener) admit(c net.Conn, ip netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.ctx.Err() != nil:
		return false
	case !allowed(l.allow, ip):
		l.log.Debug("public database: address not allowed", "ip", ip)
		return false
	case len(l.conns) >= dbExposeMaxConns || l.perIP[ip] >= dbExposeMaxConnsPerIP:
		l.log.Warn("public database: too many connections", "ip", ip)
		return false
	}
	l.conns[c] = ip
	l.perIP[ip]++
	return true
}

func (l *dbListener) release(c net.Conn, ip netip.Addr) {
	c.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, c)
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

// errDial marks a failure to reach the database's container.
var errDial = errors.New("cannot reach the container")

// dial connects to the database's current container.
func (l *dbListener) dial() (net.Conn, error) {
	c, err := l.dialContainer()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDial, err)
	}
	return c, nil
}

func (l *dbListener) dialContainer() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(l.ctx, dbExposeDialTimeout)
	defer cancel()
	l.mu.Lock()
	addr, e := l.addr, l.entry
	if time.Now().After(l.addrTill) {
		addr = ""
	}
	l.mu.Unlock()
	if addr == "" {
		var err error
		if addr, err = l.target(ctx, e.AppID, e.Service, e.TargetPort); err != nil {
			return nil, err
		}
		l.mu.Lock()
		l.addr, l.addrTill = addr, time.Now().Add(dbExposeTargetTTL)
		l.mu.Unlock()
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		l.mu.Lock()
		l.addr = "" // the container may have been replaced
		l.mu.Unlock()
	}
	return c, err
}
