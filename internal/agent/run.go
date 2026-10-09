package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/pki"
)

const (
	metricsInterval = 10 * time.Second
	minBackoff      = time.Second
	maxBackoff      = time.Minute
	// Renew when less than a third of the certificate lifetime remains.
	renewBefore = pki.AgentValidity / 3
)

// Config is the host-level agent configuration.
type Config struct {
	DataDir string
	// ProxyDir is the data directory of `basalt proxy` on this host.
	ProxyDir string
	// SitesRoot confines every static site root.
	SitesRoot     string
	HTTPPort      int
	HTTPSPort     int
	ACMEDirectory string
}

type Agent struct {
	dir   string
	log   *slog.Logger
	proxy *proxyManager

	mu sync.Mutex
	id *identity

	// MetricsInterval and RenewBefore can be changed in tests.
	MetricsInterval time.Duration
	RenewBefore     time.Duration
	collector       *collector
}

func New(cfg Config, log *slog.Logger) (*Agent, error) {
	id, err := loadIdentity(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Agent{
		dir: cfg.DataDir, log: log, id: id,
		proxy:           newProxyManager(cfg, log.With("subsystem", "proxy")),
		MetricsInterval: metricsInterval,
		RenewBefore:     renewBefore,
		collector:       newCollector(),
	}, nil
}

func (a *Agent) ServerID() int64 { return a.identity().ServerID }

func (a *Agent) identity() *identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

// Run keeps a connection to the panel open until ctx is cancelled,
// reconnecting with jittered exponential backoff.
func (a *Agent) Run(ctx context.Context) error {
	// The proxy is managed independently of the panel connection.
	go a.proxy.run(ctx)

	backoff := minBackoff
	for {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > maxBackoff {
			backoff = minBackoff // the session was healthy for a while
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		a.log.Warn("disconnected from panel", "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (a *Agent) httpClient() *http.Client {
	id := a.identity()
	roots := x509.NewCertPool()
	roots.AddCert(id.ca)
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	return &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    roots,
			// Verify the panel by its fixed certificate name, not the address
			// we dialled: the panel may be reached by IP or any hostname.
			ServerName: pki.PanelServerName,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				c := a.identity().tlsCertificate()
				return &c, nil
			},
		},
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}}
}

// session runs one connection until it fails.
func (a *Agent) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.identity().PanelURL+agentproto.PathConnect, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("panel rejected connection (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	peer := agentproto.NewPeer(resp.Body, pw, nil, a.handle)
	runErr := make(chan error, 1)
	go func() { runErr <- peer.Run(ctx) }()
	a.log.Info("connected to panel", "panel", a.identity().PanelURL, "server_id", a.identity().ServerID)

	if err := peer.Notify(agentproto.TypeHello, hostFacts()); err != nil {
		return err
	}
	go a.maybeRenew(ctx, peer)
	go a.reportMetrics(ctx, peer)

	return <-runErr
}

func (a *Agent) handle(ctx context.Context, typ string, payload json.RawMessage) (any, error) {
	switch typ {
	case agentproto.TypePing:
		return agentproto.PingResponse{AgentTime: time.Now()}, nil
	case agentproto.TypeProxyApply:
		var d agentproto.ProxyApply
		if err := json.Unmarshal(payload, &d); err != nil {
			return nil, err
		}
		id, err := a.proxy.Apply(ctx, d)
		if err != nil {
			return nil, err
		}
		return agentproto.ProxyApplyResponse{ConfigID: id}, nil
	default:
		return nil, fmt.Errorf("unsupported request %q", typ)
	}
}

func (a *Agent) reportMetrics(ctx context.Context, peer *agentproto.Peer) {
	t := time.NewTicker(a.MetricsInterval)
	defer t.Stop()
	for {
		m, err := a.collector.collect(a.dir)
		if err != nil {
			a.log.Warn("collect metrics", "err", err)
		} else if err := peer.Notify(agentproto.TypeMetrics, m); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-peer.Done():
			return
		case <-t.C:
		}
	}
}

// maybeRenew rotates the key and certificate when the current one nears
// expiry. The new pair is used from the next connection on.
func (a *Agent) maybeRenew(ctx context.Context, peer *agentproto.Peer) {
	if time.Until(a.identity().cert.NotAfter) > a.RenewBefore {
		return
	}
	if err := a.renew(ctx, peer); err != nil {
		a.log.Error("certificate renewal failed", "err", err, "expires", a.identity().cert.NotAfter)
	}
}

func (a *Agent) renew(ctx context.Context, peer *agentproto.Peer) error {
	key, err := pki.NewKey()
	if err != nil {
		return err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return err
	}
	var resp agentproto.RenewCertResponse
	if err := peer.Call(ctx, agentproto.TypeRenewCert, agentproto.RenewCertRequest{CSR: csr}, &resp); err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(resp.Cert)
	if err != nil {
		return err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return errors.New("renewed certificate does not match the new key")
	}
	if err := saveCredentials(a.dir, key, resp.Cert); err != nil {
		return err
	}
	a.mu.Lock()
	id := *a.id
	id.key, id.cert = key, cert
	a.id = &id
	a.mu.Unlock()
	a.log.Info("certificate renewed", "expires", cert.NotAfter)
	return nil
}

func hostFacts() agentproto.Hello {
	h := agentproto.Hello{
		AgentVersion: buildinfo.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		CPUCount:     runtime.NumCPU(),
	}
	h.Hostname, _ = os.Hostname()
	h.Kernel, h.Distro, h.MemTotal = platformFacts()
	return h
}
