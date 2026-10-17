package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/mailcheck"
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
	// ACMECARoots are PEM files trusted for the ACME server's TLS
	// certificate; ACMEResolvers find DNS-01 zones (see proxy.BuildOptions).
	ACMECARoots   []string
	ACMEResolvers []string
	// Resolvers are recursive DNS servers for mail deliverability checks
	// (empty: the host's).
	Resolvers []string
	// MailCheckProbes replaces the SMTP servers the outgoing port 25 check
	// connects to (tests).
	MailCheckProbes []string
	// SFTPListen is the SFTP server address; empty disables SFTP.
	SFTPListen string
	// DNSListen is the authoritative DNS address (UDP and TCP); empty
	// disables DNS.
	DNSListen string
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
	// GitCAFile is an extra CA bundle trusted for https git remotes (a
	// self-hosted git server with a private CA).
	GitCAFile string
	// Restic is the restic binary backups run with ("" disables backups).
	Restic string
	// PanelDomain and PanelUpstream publish the panel itself through this
	// host's proxy (set on the panel's own host). The route exists even
	// before the panel has sent any state, so the UI is reachable at once.
	PanelDomain   string
	PanelUpstream string
	// ReleaseKey is the key release checksums must be signed with (default:
	// the Basalt release key).
	ReleaseKey string
	// UpdateExe and Restart replace, in tests, the binary an update
	// replaces and how the services are restarted (systemctl).
	UpdateExe string
	Restart   func(units []string) error
}

type Agent struct {
	dir     string
	log     *slog.Logger
	proxy   *proxyManager
	sites   *sitesManager
	sftp    *sftpManager
	dns     *dnsManager
	acme    *acmeRelay
	apps    *appsManager
	backups *backupManager
	mysql   string
	docker  string

	health      *healthManager
	mailCheck   *mailcheck.Checker
	mailCheckMu sync.Mutex // one deliverability check at a time

	mu sync.Mutex
	id *identity

	// MetricsInterval and RenewBefore can be changed in tests.
	MetricsInterval time.Duration
	RenewBefore     time.Duration
	collector       *collector
	updater         *updater
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
	backups := newBackupManager(cfg, sites, apps, log.With("subsystem", "backup"))
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
		dns:             newDNSManager(cfg, log.With("subsystem", "dns")),
		acme:            newACMERelay(cfg, log.With("subsystem", "acme")),
		health:          newHealthManager(log.With("subsystem", "health")),
		mailCheck:       &mailcheck.Checker{Resolvers: cfg.Resolvers, SMTPProbes: cfg.MailCheckProbes},
		apps:            apps,
		backups:         backups,
		mysql:           cfg.MySQLSocket,
		docker:          cfg.DockerSocket,
		proxy:           proxy,
		MetricsInterval: metricsInterval,
		RenewBefore:     renewBefore,
		collector:       newCollector(),
		updater:         newUpdater(cfg, log.With("subsystem", "update")),
	}, nil
}

func (a *Agent) ServerID() int64 { return a.identity().ServerID }

func (a *Agent) identity() *identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

// errRemoved: the panel answered that this server no longer exists.
var errRemoved = errors.New("the server was removed from the panel")

const removedHelp = "sites keep running; to remove Basalt from this server run the installer with \"uninstall\", to connect it again enroll it with a new token"

// Run keeps a connection to the panel open until ctx is cancelled,
// reconnecting with jittered exponential backoff.
func (a *Agent) Run(ctx context.Context) error {
	// The proxy is managed independently of the panel connection.
	go a.proxy.run(ctx)
	go a.sites.janitor(ctx)
	go a.sftp.run(ctx)
	go a.dns.run(ctx)
	go a.acme.run(ctx)
	go a.apps.run(ctx)
	defer a.health.stop()

	if b, err := os.ReadFile(filepath.Join(a.dir, fileRemoved)); err == nil {
		a.log.Error("this server was removed from the panel; not connecting", "removed", strings.TrimSpace(string(b)), "help", removedHelp)
		<-ctx.Done()
		return nil
	}
	backoff := minBackoff
	for {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errRemoved) {
			// Retrying cannot help: the panel forgot this server. Sites,
			// SFTP and DNS go on serving what they had.
			if werr := os.WriteFile(filepath.Join(a.dir, fileRemoved), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); werr != nil {
				a.log.Warn("record removal", "err", werr)
			}
			a.log.Error("this server was removed from the panel; no longer connecting", "help", removedHelp)
			<-ctx.Done()
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
	if resp.StatusCode == http.StatusGone {
		return errRemoved
	}
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
	hello.Docker, hello.DockerSecurity = a.dockerVersion(ctx)
	hello.Restic = a.backups.Version(ctx)
	hello.Updatable = a.updater.updatable()
	hello.DNSPort, hello.DNSError = a.dns.Status()
	hello.HTTPSPort = a.proxy.opts.HTTPSPort
	if err := peer.Notify(agentproto.TypeHello, hello); err != nil {
		return err
	}
	a.acme.setPanel(peer)
	defer a.acme.setPanel(nil)
	go a.maybeRenew(ctx, peer)
	go a.reportMetrics(ctx, peer)
	go a.health.report(ctx, peer)

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
			return struct{}{}, publicError(a.sites.Ensure(ctx, req.SiteID, req.PHPVersion, req.PHP))
		}
		return struct{}{}, publicError(a.sites.Delete(ctx, req.SiteID))
	case agentproto.TypeSFTPApply:
		var st agentproto.SFTPApply
		if err := json.Unmarshal(payload, &st); err != nil {
			return nil, err
		}
		return struct{}{}, a.sftp.Apply(st)
	case agentproto.TypeSFTPBans:
		var req agentproto.SFTPBans
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.sftp.Bans(req)
	case agentproto.TypeMailAPI:
		var req agentproto.MailAPI
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.apps.MailAPI(ctx, req)
	case agentproto.TypeMailCheck:
		var req agentproto.MailCheck
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.MailCheck(ctx, req)
	case agentproto.TypeCertStatus:
		var req agentproto.CertStatus
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return certStatus(a.proxy.opts.Dir, req)
	case agentproto.TypeMailCert:
		var req agentproto.MailCert
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.MailCert(ctx, req)
	case agentproto.TypeHealthApply:
		var req agentproto.HealthApply
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.health.Apply(ctx, req)
	case agentproto.TypeDNSApply:
		var st agentproto.DNSApply
		if err := json.Unmarshal(payload, &st); err != nil {
			return nil, err
		}
		return struct{}{}, a.dns.Apply(st)
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
		return a.apps.Deploy(ctx, spec, agentproto.StreamSender(ctx)), nil
	case agentproto.TypeAppLogsFollow:
		var req agentproto.AppRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		send := agentproto.StreamSender(ctx)
		if send == nil {
			return nil, errors.New("following logs needs a streaming request")
		}
		return struct{}{}, a.apps.FollowLogs(ctx, req.AppID, req.Service, req.Tail, func(s string) error {
			return send(agentproto.LogChunk{Text: s})
		})
	case agentproto.TypeAppDelete, agentproto.TypeAppStatus, agentproto.TypeAppLogs:
		var req agentproto.AppRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		switch typ {
		case agentproto.TypeAppDelete:
			return struct{}{}, a.apps.Delete(ctx, req.AppID, req.DeleteVolumes, req.PruneImages)
		case agentproto.TypeAppStatus:
			return a.apps.Status(ctx, req.AppID, req.Service)
		}
		log, err := a.apps.Logs(ctx, req.AppID, req.Service, req.Tail)
		return agentproto.AppLogsResponse{Log: log}, err
	case agentproto.TypeBackupCheck:
		var req agentproto.BackupRepo
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.backups.Check(ctx, req)
	case agentproto.TypeBackupRun:
		var req agentproto.BackupRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.backups.Run(ctx, req, agentproto.StreamSender(ctx)), nil
	case agentproto.TypeBackupSnapshots:
		var req agentproto.SnapshotsRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.backups.Snapshots(ctx, req)
	case agentproto.TypeBackupVerify:
		var req agentproto.VerifyRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.backups.Verify(ctx, req, agentproto.StreamSender(ctx)), nil
	case agentproto.TypeBackupRestore:
		var req agentproto.RestoreRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.backups.Restore(ctx, req, agentproto.StreamSender(ctx)), nil
	case agentproto.TypeBackupBrowse, agentproto.TypeBackupExport, agentproto.TypeBackupRestoreCopy:
		var req agentproto.SnapshotFilesRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		var resp any
		var err error
		switch typ {
		case agentproto.TypeBackupBrowse:
			resp, err = a.backups.Browse(ctx, req)
		case agentproto.TypeBackupExport:
			resp, err = a.backups.Export(ctx, req)
		default:
			resp, err = a.backups.RestoreCopy(ctx, req)
		}
		return resp, publicError(err)
	case agentproto.TypeBackupExportRead:
		var req agentproto.ExportRead
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return a.backups.ExportRead(req)
	case agentproto.TypeBackupExportClose:
		var req agentproto.ExportClose
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.backups.ExportClose(req)
	case agentproto.TypeUpdatePrepare:
		var req agentproto.UpdatePrepare
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.updater.prepare(ctx, a.httpClient(), a.identity().PanelURL, req)
	case agentproto.TypeUpdateActivate:
		var req agentproto.UpdateActivate
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.updater.activate(req)
	case agentproto.TypeAppAttach:
		var req agentproto.AppAttach
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return struct{}{}, a.apps.Attach(ctx, req)
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
		if err == nil {
			sctx, cancel := context.WithTimeout(ctx, a.MetricsInterval)
			m.Apps = a.apps.Stats(sctx)
			cancel()
		}
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
	h.PublicIPs = publicIPs()
	return h
}

// publicIPs lists the public addresses on the host's interfaces. A server
// behind NAT has none; the panel then uses the address it connects from,
// or what an admin set.
func publicIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	proc, _ := os.ReadFile("/proc/net/if_inet6")
	unstable := unstableIPv6(string(proc))
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(ipn.IP); ok && dns.PublicAddr(ip) && !unstable[ip.Unmap()] {
			out = append(out, ip.Unmap().String())
		}
	}
	return out
}

// unstableIPv6 parses /proc/net/if_inet6 (Linux) for addresses that must
// not be published: temporary (privacy) addresses change every few hours,
// deprecated ones are being replaced, and ones that failed duplicate
// detection are not used.
func unstableIPv6(proc string) map[netip.Addr]bool {
	const (
		ifaTemporary  = 0x01
		ifaDADFailed  = 0x08
		ifaDeprecated = 0x20
	)
	out := map[netip.Addr]bool{}
	for line := range strings.Lines(proc) {
		f := strings.Fields(line)
		if len(f) < 6 || len(f[0]) != 32 {
			continue
		}
		raw, err := hex.DecodeString(f[0])
		flags, ferr := strconv.ParseUint(f[4], 16, 32)
		if err != nil || ferr != nil {
			continue
		}
		if flags&(ifaTemporary|ifaDADFailed|ifaDeprecated) != 0 {
			out[netip.AddrFrom16([16]byte(raw))] = true
		}
	}
	return out
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

// dockerVersion returns the version of the local Docker Engine and how it
// isolates containers ("" and nil without a usable daemon).
func (a *Agent) dockerVersion(ctx context.Context) (string, *docker.Security) {
	if a.docker == "" || !docker.Available(a.docker) {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dc := docker.New(a.docker)
	v, err := dc.Version(ctx)
	if err != nil {
		a.log.Warn("Docker socket present but not usable", "err", err)
		return "", nil
	}
	info, err := dc.Info(ctx)
	if err != nil {
		a.log.Warn("Docker info", "err", err)
		return v.Version, nil
	}
	sec := info.Security()
	return v.Version, &sec
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
