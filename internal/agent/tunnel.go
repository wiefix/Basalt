package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
)

// The tunnel lets agents of other servers reach this server's local
// services (agentproto.PeerApply), and this server's loopback listeners
// reach theirs. Both ends present the certificates the panel issued them
// and accept only certificates of the panel's CA; the client also checks
// that it reached the server it meant to.

const (
	tunnelHandshake = 10 * time.Second
	maxTunnelConns  = 256
)

type tunnelManager struct {
	log    *slog.Logger
	listen string
	// cert and ca give the agent's current certificate and the panel's
	// CA (the certificate is renewed while the agent runs).
	cert   func() tls.Certificate
	ca     func() *x509.Certificate
	target map[string]func(ctx context.Context) (net.Conn, error) // by service

	mu       sync.Mutex
	revision int64
	allowed  map[string][]int64 // service -> servers
	forwards map[string]*tunnelForward
	port     int
	err      string
	conns    chan struct{} // tunnel connections being served
}

type tunnelForward struct {
	agentproto.PeerForward
	ln net.Listener
}

func newTunnelManager(cfg Config, cert func() tls.Certificate, ca func() *x509.Certificate, log *slog.Logger) *tunnelManager {
	m := &tunnelManager{log: log, listen: cfg.PeerListen, cert: cert, ca: ca,
		target:   map[string]func(context.Context) (net.Conn, error){},
		allowed:  map[string][]int64{},
		forwards: map[string]*tunnelForward{},
		conns:    make(chan struct{}, maxTunnelConns),
	}
	if cfg.MySQLSocket != "" {
		m.target[agentproto.ServiceMariaDB] = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cfg.MySQLSocket)
		}
	}
	return m
}

// Status is the port the tunnel listens on (0: off) and why it could not.
func (m *tunnelManager) Status() (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port, m.err
}

func (m *tunnelManager) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(m.ca())
	return p
}

// serverConfig accepts clients with a certificate of the panel's CA.
func (m *tunnelManager) serverConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := m.cert()
			return &c, nil
		},
		ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := m.verify(raw, 0)
			return err
		},
	}
}

// verify checks a peer's certificate chain against the panel's CA (agents'
// certificates are for client authentication, whichever end they are on)
// and returns its server id, which must be want unless want is 0.
func (m *tunnelManager) verify(raw [][]byte, want int64) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return 0, err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: m.pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return 0, err
	}
	id, err := pki.AgentID(leaf)
	if err != nil {
		return 0, err
	}
	if want != 0 && id != want {
		return 0, fmt.Errorf("reached server %d, not %d", id, want)
	}
	return id, nil
}

// open listens on the tunnel's address (before the agent says hello, so
// the panel learns the port); nil if off or impossible.
func (m *tunnelManager) open() net.Listener {
	if m.listen == "" {
		return nil
	}
	ln, err := net.Listen("tcp", m.listen)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.err = err.Error()
		m.log.Error("tunnel cannot listen", "addr", m.listen, "err", err)
		return nil
	}
	m.port = ln.Addr().(*net.TCPAddr).Port
	m.log.Info("tunnel listening", "addr", ln.Addr().String())
	return ln
}

// run serves the tunnel on ln (nil: only closes the forwards at the end).
func (m *tunnelManager) run(ctx context.Context, ln net.Listener) {
	if ln == nil {
		<-ctx.Done()
		m.mu.Lock()
		for _, f := range m.forwards {
			f.ln.Close()
		}
		m.mu.Unlock()
		return
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		m.mu.Lock()
		for _, f := range m.forwards {
			f.ln.Close()
		}
		m.mu.Unlock()
	}()
	cfg := m.serverConfig()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case m.conns <- struct{}{}:
		default:
			c.Close() // too many
			continue
		}
		go func() {
			defer func() { <-m.conns }()
			m.serve(ctx, tls.Server(c, cfg))
		}()
	}
}

// serve answers one tunnel connection.
func (m *tunnelManager) serve(ctx context.Context, c *tls.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(tunnelHandshake))
	if err := c.HandshakeContext(ctx); err != nil {
		return
	}
	peer, err := m.verify([][]byte{c.ConnectionState().PeerCertificates[0].Raw}, 0)
	if err != nil {
		return
	}
	r := bufio.NewReaderSize(c, 1024)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return
	}
	var hello agentproto.TunnelHello
	reply := func(e string) {
		b, _ := json.Marshal(agentproto.TunnelReply{OK: e == "", Error: e})
		c.Write(append(b, '\n'))
	}
	if json.Unmarshal(line, &hello) != nil {
		reply("bad request")
		return
	}
	m.mu.Lock()
	ok := slices.Contains(m.allowed[hello.Service], peer)
	m.mu.Unlock()
	dial := m.target[hello.Service]
	if !ok || dial == nil {
		m.log.Warn("tunnel refused", "server_id", peer, "service", hello.Service)
		reply("not allowed")
		return
	}
	dctx, cancel := context.WithTimeout(ctx, tunnelHandshake)
	local, err := dial(dctx)
	cancel()
	if err != nil {
		reply("the service is not available")
		return
	}
	defer local.Close()
	reply("")
	c.SetDeadline(time.Time{})
	splice(c, r, local)
}

// splice copies both ways until either side closes; buffered holds what was
// read from a ahead of the copy.
func splice(a net.Conn, buffered io.Reader, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(b, buffered)
		if cw, ok := b.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(a, b)
		if cw, ok := a.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// apply replaces the tunnel setup; an older revision is ignored.
func (m *tunnelManager) apply(ctx context.Context, req agentproto.PeerApply) error {
	if err := req.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Revision < m.revision {
		return nil
	}
	m.revision = req.Revision
	m.allowed = req.Services
	if m.allowed == nil {
		m.allowed = map[string][]int64{}
	}
	want := map[string]agentproto.PeerForward{}
	for _, f := range req.Forwards {
		want[f.Listen] = f
	}
	for listen, f := range m.forwards {
		if w, ok := want[listen]; !ok || w != f.PeerForward {
			f.ln.Close()
			delete(m.forwards, listen)
		}
	}
	var errs []error
	for listen, f := range want {
		if _, ok := m.forwards[listen]; ok {
			continue
		}
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			errs = append(errs, fmt.Errorf("forward %s: %w", listen, err))
			continue
		}
		fw := &tunnelForward{PeerForward: f, ln: ln}
		m.forwards[listen] = fw
		go m.forward(ctx, fw)
	}
	return errors.Join(errs...)
}

// forward accepts local connections and carries each through the tunnel of
// the forward's server.
func (m *tunnelManager) forward(ctx context.Context, f *tunnelForward) {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return // closed
		}
		go func() {
			defer c.Close()
			remote, _, err := m.dial(ctx, f.PeerForward)
			if err != nil {
				m.log.Warn("tunnel forward", "listen", f.Listen, "server_id", f.ServerID, "err", err)
				return
			}
			defer remote.Close()
			splice(c, c, remote)
		}()
	}
}

// dial opens a tunnel to f's service; the reader holds what came after the
// server's reply.
func (m *tunnelManager) dial(ctx context.Context, f agentproto.PeerForward) (net.Conn, io.Reader, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c := m.cert()
			return &c, nil
		},
		// The chain and the server's identity are checked below, against
		// the panel's CA (agents' certificates carry no host names).
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := m.verify(raw, f.ServerID)
			return err
		},
	}
	dctx, cancel := context.WithTimeout(ctx, tunnelHandshake)
	defer cancel()
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: cfg}
	nc, err := d.DialContext(dctx, "tcp", f.Addr)
	if err != nil {
		return nil, nil, err
	}
	c := nc.(*tls.Conn)
	c.SetDeadline(time.Now().Add(tunnelHandshake))
	b, _ := json.Marshal(agentproto.TunnelHello{Service: f.Service})
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, nil, err
	}
	r := bufio.NewReaderSize(c, 1024)
	line, err := r.ReadSlice('\n')
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	var reply agentproto.TunnelReply
	if err := json.Unmarshal(line, &reply); err != nil || !reply.OK {
		c.Close()
		if reply.Error == "" {
			reply.Error = "bad reply"
		}
		return nil, nil, errors.New("server " + strconv.FormatInt(f.ServerID, 10) + ": " + reply.Error)
	}
	c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: r}, r, nil
}

// bufferedConn reads through a reader that may hold bytes already read.
type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
