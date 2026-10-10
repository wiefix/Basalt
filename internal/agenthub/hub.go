// Package agenthub is the control-plane side of the agent channel: it
// enrolls new servers, accepts their mutual-TLS connections and lets the rest
// of the panel call into connected agents.
package agenthub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/store"
)

const (
	metricsRetention = 48 * time.Hour
	writeTimeout     = 15 * time.Second
	maxEnrollBody    = 64 << 10
)

var ErrAgentOffline = errors.New("agent is not connected")

type Hub struct {
	log *slog.Logger
	db  *store.DB
	ca  *pki.CA
	now func() time.Time

	panelCerts    *pki.PanelCertificates
	enrollLimiter *auth.Limiter

	mu        sync.Mutex
	conns     map[int64]*conn
	onConnect func(ctx context.Context, serverID int64)

	installScript []byte
	binariesDir   string
}

// OnConnect registers a callback run (in its own goroutine) whenever an
// agent connects, e.g. to push desired state. Call before serving.
func (h *Hub) OnConnect(f func(ctx context.Context, serverID int64)) { h.onConnect = f }

type conn struct {
	peer   *agentproto.Peer
	cancel context.CancelFunc
}

func New(log *slog.Logger, db *store.DB, ca *pki.CA, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{
		log:           log,
		db:            db,
		ca:            ca,
		now:           now,
		panelCerts:    ca.PanelCertificates(now),
		enrollLimiter: auth.NewLimiter(10, 6*time.Second).WithClock(now),
		conns:         map[int64]*conn{},
	}
}

// CAFingerprint is pinned by agents during enrollment.
func (h *Hub) CAFingerprint() string { return h.ca.Fingerprint() }

// TLSConfig requests (but does not require) a client certificate: enrollment
// happens before the agent has one. The connect endpoint enforces it.
func (h *Hub) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: h.panelCerts.GetCertificate,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      h.ca.Pool(),
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentproto.PathEnroll, h.handleEnroll)
	mux.HandleFunc("POST "+agentproto.PathConnect, h.handleConnect)
	h.registerInstaller(mux)
	return mux
}

// Serve runs the agent listener until ctx is cancelled.
func (h *Hub) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("agent listener on %s: %w", addr, err)
	}
	return h.ServeListener(ctx, ln)
}

// ServeListener is Serve on an existing listener.
func (h *Hub) ServeListener(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           h.Handler(),
		TLSConfig:         h.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(h.log.Handler(), slog.LevelDebug),
		// Request contexts derive from ctx, so agent streams end on shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go h.pruneLoop(ctx)

	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	h.log.Info("agent listener ready", "addr", ln.Addr().String(), "ca_sha256", h.ca.Fingerprint())

	select {
	case err := <-errc:
		return fmt.Errorf("agent listener: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close()
	}
	return nil
}

func (h *Hub) pruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := h.db.DeleteMetricsBefore(ctx, h.now().Add(-metricsRetention)); err != nil && ctx.Err() == nil {
			h.log.Error("prune metrics", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- enrollment ---

func (h *Hub) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !h.enrollLimiter.Allow(ip) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var req agentproto.EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	id, err := h.db.ConsumeEnrollToken(r.Context(), auth.HashToken(req.Token), h.now())
	if errors.Is(err, store.ErrNotFound) {
		h.log.Warn("enrollment rejected: invalid or expired token", "ip", ip)
		http.Error(w, "invalid or expired enrollment token", http.StatusForbidden)
		return
	}
	if err != nil {
		h.internalError(w, "consume enroll token", err)
		return
	}
	certDER, err := h.issue(r.Context(), id, req.CSR)
	if err != nil {
		// The token is spent; an admin can issue a new one for this server.
		h.log.Warn("enrollment failed", "server_id", id, "err", err)
		http.Error(w, "certificate request rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.Audit(r.Context(), nil, "server.enrolled", ip, map[string]any{"server_id": id}); err != nil {
		h.log.Error("audit", "err", err)
	}
	h.log.Info("server enrolled", "server_id", id, "ip", ip)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(agentproto.EnrollResponse{ServerID: id, Cert: certDER, CACert: h.ca.Cert.Raw})
}

func (h *Hub) issue(ctx context.Context, serverID int64, csr []byte) ([]byte, error) {
	now := h.now()
	der, err := h.ca.SignAgent(csr, serverID, now)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := h.db.SetServerCert(ctx, serverID, pki.Fingerprint(der), cert.NotAfter, now); err != nil {
		return nil, err
	}
	return der, nil
}

// --- agent connections ---

func (h *Hub) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	if r.ProtoMajor != 2 {
		http.Error(w, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
		return
	}
	leaf := r.TLS.VerifiedChains[0][0]
	id, err := pki.AgentID(leaf)
	if err != nil {
		http.Error(w, "invalid agent certificate", http.StatusForbidden)
		return
	}
	srv, err := h.db.ServerByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "server has been removed", http.StatusForbidden)
		return
	}
	if err != nil {
		h.internalError(w, "load server", err)
		return
	}
	// Only the current (or, during renewal, the previous) certificate is
	// accepted. This is how removed or re-enrolled servers are revoked.
	fp := pki.Fingerprint(leaf.Raw)
	if fp != srv.CertFP && fp != srv.PrevCertFP {
		h.log.Warn("agent presented a revoked certificate", "server_id", id, "ip", remoteIP(r))
		http.Error(w, "certificate revoked", http.StatusForbidden)
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ip := remoteIP(r)
	peer := agentproto.NewPeer(r.Body, &deadlineWriter{w: w, rc: rc, timeout: writeTimeout}, nil,
		func(ctx context.Context, typ string, payload json.RawMessage) (any, error) {
			return h.handleAgentMessage(ctx, id, ip, typ, payload)
		})

	c := &conn{peer: peer, cancel: cancel}
	h.mu.Lock()
	if old := h.conns[id]; old != nil {
		old.cancel() // a reconnect replaces the stale stream
	}
	h.conns[id] = c
	h.mu.Unlock()
	h.log.Info("agent connected", "server_id", id, "ip", ip)
	if h.onConnect != nil {
		go h.onConnect(ctx, id)
	}

	err = peer.Run(ctx)

	h.mu.Lock()
	if h.conns[id] == c {
		delete(h.conns, id)
	}
	h.mu.Unlock()
	h.log.Info("agent disconnected", "server_id", id, "reason", err)
}

func (h *Hub) handleAgentMessage(ctx context.Context, id int64, ip, typ string, payload json.RawMessage) (any, error) {
	switch typ {
	case agentproto.TypeHello:
		var m agentproto.Hello
		if err := json.Unmarshal(payload, &m); err != nil {
			return nil, err
		}
		now := h.now()
		if err := h.db.SetServerSFTP(ctx, id, max(0, min(m.SFTPPort, 65535)), clean(m.SFTPFingerprint)); err != nil {
			return nil, err
		}
		if err := h.db.SetServerMariaDB(ctx, id, clean(m.MariaDB)); err != nil {
			return nil, err
		}
		return nil, h.db.UpdateServerHello(ctx, id, store.Server{
			Hostname: clean(m.Hostname), OS: clean(m.OS), Arch: clean(m.Arch), Kernel: clean(m.Kernel),
			Distro: clean(m.Distro), AgentVersion: clean(m.AgentVersion),
			CPUCount: max(0, m.CPUCount), MemTotal: m.MemTotal, LastIP: ip, LastSeen: &now,
			PHPVersions: cleanVersions(m.PHPVersions), Isolated: m.Isolated, Docker: clean(m.Docker),
		})
	case agentproto.TypeMetrics:
		var m agentproto.Metrics
		if err := json.Unmarshal(payload, &m); err != nil {
			return nil, err
		}
		// Timestamp with the panel clock: agent clocks may be wrong.
		return nil, h.db.InsertMetrics(ctx, id, store.ServerMetrics{
			At: h.now(), CPUPct: clamp(m.CPUPct, 0, 100), MemUsed: m.MemUsed, MemTotal: m.MemTotal,
			DiskUsed: m.DiskUsed, DiskTotal: m.DiskTotal, Load1: clamp(m.Load1, 0, 1e6),
			Load5: clamp(m.Load5, 0, 1e6), Load15: clamp(m.Load15, 0, 1e6), Uptime: m.Uptime,
		})
	case agentproto.TypeRenewCert:
		var req agentproto.RenewCertRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		der, err := h.issue(ctx, id, req.CSR)
		if err != nil {
			return nil, err
		}
		h.log.Info("agent certificate renewed", "server_id", id)
		return agentproto.RenewCertResponse{Cert: der}, nil
	default:
		return nil, fmt.Errorf("unknown message type %q", typ)
	}
}

// Connected reports whether the agent of server id has a live stream.
func (h *Hub) Connected(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[id] != nil
}

// Call sends a request to a connected agent.
func (h *Hub) Call(ctx context.Context, id int64, msgType string, req, resp any) error {
	return h.Stream(ctx, id, msgType, req, resp, nil)
}

// Stream sends a request whose answer is streamed: fn receives the frames
// in order before the reply is decoded into resp.
func (h *Hub) Stream(ctx context.Context, id int64, msgType string, req, resp any, fn func(json.RawMessage) error) error {
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c == nil {
		return ErrAgentOffline
	}
	return c.peer.Stream(ctx, msgType, req, resp, fn)
}

// Disconnect drops the agent's stream, e.g. after the server was removed.
func (h *Hub) Disconnect(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.conns[id]; c != nil {
		c.cancel()
		delete(h.conns, id)
	}
}

func (h *Hub) internalError(w http.ResponseWriter, what string, err error) {
	h.log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// deadlineWriter writes and flushes each frame within a deadline, so a
// stalled agent cannot block the writer forever (HTTP/2 flow control would
// otherwise wait indefinitely). The deadline is lifted after the write: an
// HTTP/2 write deadline left armed resets the stream when it passes, even
// with no write in progress, which dropped idle agents.
type deadlineWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	defer d.rc.SetWriteDeadline(time.Time{})
	n, err := d.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, d.rc.Flush()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clean bounds agent-reported strings before they reach the database and UI.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	for len(s) > 128 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func clamp(v, lo, hi float64) float64 {
	if v != v { // NaN
		return lo
	}
	return min(max(v, lo), hi)
}

var phpVersionRE = regexp.MustCompile(`^[5-9]\.[0-9]$`)

func cleanVersions(vs []string) []string {
	out := []string{}
	for _, v := range vs {
		if phpVersionRE.MatchString(v) && len(out) < 20 {
			out = append(out, v)
		}
	}
	return out
}
