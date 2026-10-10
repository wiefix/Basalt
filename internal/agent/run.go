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
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/mariadb"
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
	// SFTPListen is the SFTP server address; empty disables SFTP.
	SFTPListen string
	// Isolate runs each site as its own system user (Linux, root only).
	Isolate bool
	// WWWGroup is the group shared by site directories and the proxy.
	WWWGroup string
	// ProxyUser is the system user `basalt proxy` runs as ("" if root).
	ProxyUser string
	// PHPRunDir holds PHP-FPM pool sockets.
	PHPRunDir string
	// MySQLSocket is the local MariaDB/MySQL socket ("" disables databases).
	MySQLSocket string
	// DockerSocket is the local Docker Engine socket ("" disables apps).
	DockerSocket string
	// PanelDomain and PanelUpstream publish the panel itself through this
	// host's proxy (set on the panel's own host). The route exists even
	// before the panel has sent any state, so the UI is reachable at once.
	PanelDomain   string
	PanelUpstream string
}

type Agent struct {
	dir    string
	log    *slog.Logger
	proxy  *proxyManager
	sites  *sitesManager
	sftp   *sftpManager
	apps   *appsManager
	mysql  string
	docker string

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
	if cfg.Isolate && (runtime.GOOS != "linux" || os.Geteuid() != 0) {
		return nil, errors.New("site isolation requires Linux and running as root")
	}
	if cfg.WWWGroup == "" {
		cfg.WWWGroup = "basalt-www"
	}
	if cfg.PHPRunDir == "" {
		cfg.PHPRunDir = "/run/basalt/php"
	}
	sites := newSitesManager(cfg, log.With("subsystem", "sites"))
	sftp, err := newSFTPManager(cfg, sites, log.With("subsystem", "sftp"))
	if err != nil {
		return nil, err
	}
	apps := newAppsManager(cfg, log.With("subsystem", "apps"))
	proxy := newProxyManager(cfg, sites, apps, log.With("subsystem", "proxy"))
	apps.onChange = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		proxy.pushAppUpstreams(ctx)
	}
	return &Agent{
		dir: cfg.DataDir, log: log, id: id,
		sites:           sites,
		sftp:            sftp,
		apps:            apps,
		mysql:           cfg.MySQLSocket,
		docker:          cfg.DockerSocket,
		proxy:           proxy,
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
	go a.sites.janitor(ctx)
	go a.sftp.run(ctx)
	go a.apps.run(ctx)

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

	hello := hostFacts()
	hello.SFTPPort, hello.SFTPFingerprint = a.sftp.Port(), a.sftp.Fingerprint()
	hello.PHPVersions, hello.Isolated = a.sites.PHPVersions(), a.sites.isolate
	hello.MariaDB = a.mariadbVersion(ctx)
	hello.Docker = a.dockerVersion(ctx)
	if err := peer.Notify(agentproto.TypeHello, hello); err != nil {
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
	case agentproto.TypeSiteEnsure, agentproto.TypeSiteDelete:
		var req agentproto.SiteRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		if typ == agentproto.TypeSiteEnsure {
			return struct{}{}, publicError(a.sites.Ensure(ctx, req.SiteID, req.PHPVersion))
		}
		return struct{}{}, publicError(a.sites.Delete(ctx, req.SiteID))
	case agentproto.TypeSFTPApply:
		var st agentproto.SFTPApply
		if err := json.Unmarshal(payload, &st); err != nil {
			return nil, err
		}
		return struct{}{}, a.sftp.Apply(st)
	case agentproto.TypeDatabase:
		var req agentproto.DatabaseRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.database(ctx, req)
	case agentproto.TypeAppDeploy:
		var spec agentproto.AppSpec
		if err := json.Unmarshal(payload, &spec); err != nil {
			return nil, err
		}
		return a.apps.Deploy(ctx, spec), nil
	case agentproto.TypeAppDelete, agentproto.TypeAppStatus, agentproto.TypeAppLogs:
		var req agentproto.AppRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		switch typ {
		case agentproto.TypeAppDelete:
			return struct{}{}, a.apps.Delete(ctx, req.AppID, req.DeleteVolumes)
		case agentproto.TypeAppStatus:
			return a.apps.Status(ctx, req.AppID)
		}
		log, err := a.apps.Logs(ctx, req.AppID, req.Tail)
		return agentproto.AppLogsResponse{Log: log}, err
	case agentproto.TypeAppsSync:
		var req agentproto.AppsSync
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.apps.Sync(ctx, req.AppIDs)
	case agentproto.TypeFiles:
		var req agentproto.FilesRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.sites.handleFiles(ctx, req)
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

func (a *Agent) mariadbVersion(ctx context.Context) string {
	if a.mysql == "" || !mariadb.Available(a.mysql) {
		return ""
	}
	db, err := mariadb.Open(a.mysql)
	if err != nil {
		return ""
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := db.Version(ctx)
	if err != nil {
		a.log.Warn("MariaDB socket present but not usable", "err", err)
		return ""
	}
	return v
}

func (a *Agent) dockerVersion(ctx context.Context) string {
	if a.docker == "" || !docker.Available(a.docker) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := docker.New(a.docker).Version(ctx)
	if err != nil {
		a.log.Warn("Docker socket present but not usable", "err", err)
		return ""
	}
	return v.Version
}

func (a *Agent) database(ctx context.Context, req agentproto.DatabaseRequest) error {
	if a.mysql == "" || !mariadb.Available(a.mysql) {
		return errors.New("MariaDB is not available on this server")
	}
	db, err := mariadb.Open(a.mysql)
	if err != nil {
		return err
	}
	defer db.Close()
	switch req.Op {
	case agentproto.DBCreate:
		return db.CreateDatabase(ctx, req.Name, req.User, req.Password)
	case agentproto.DBDrop:
		return db.DropDatabase(ctx, req.Name, req.User)
	case agentproto.DBPassword:
		return db.SetPassword(ctx, req.User, req.Password)
	}
	return fmt.Errorf("unknown database operation %q", req.Op)
}
