// Package server implements the control plane HTTP API.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/store"
)

const shutdownTimeout = 15 * time.Second

type Options struct {
	Log    *slog.Logger
	DB     *store.DB
	Box    *secrets.Box
	Hasher *auth.Hasher
	// SetupToken must be presented to create the first owner account.
	// Empty once setup is complete.
	SetupToken string
	// SecureCookies sets the Secure attribute and the __Host- prefix.
	// Disable only for local development over plain HTTP.
	SecureCookies bool
	// Agents is the agent channel; AgentURL is the address agents use to
	// reach it, shown in enrollment commands.
	Agents   AgentHub
	AgentURL string
	// TrustedProxies may set X-Forwarded-For (e.g. the local proxy).
	TrustedProxies []netip.Prefix
	// SetupTokenFile, if set, receives the setup token so an installer can
	// show it; it is removed once setup completes.
	SetupTokenFile string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// DataDir is the panel's data directory (backup staging, restic cache).
	DataDir string
	// Restic is the restic binary panel backups run with.
	Restic string
	// ImagesKept is how many images of recent deployments each app keeps
	// on its server (default 3).
	ImagesKept int
	// MailHostPorts publishes mail ports elsewhere on the host (tests);
	// by default port 25 is 25 and so on.
	MailHostPorts map[int]int
	// MailCheckEvery is how often the panel checks mail deliverability
	// (default a day; negative: only on request).
	MailCheckEvery time.Duration
	// Catalog replaces the built-in app catalog (tests).
	Catalog func() ([]*catalog.Template, error)
	// TelegramAPI replaces https://api.telegram.org, and NotifyRetries the
	// pauses before delivery attempts (tests). With TelegramAPI set, Slack,
	// Discord and Telegram deliveries may reach private addresses.
	TelegramAPI   string
	NotifyRetries []time.Duration
	// OfflineAfter is how long a server's agent stays away before a
	// notification (default 5 minutes).
	OfflineAfter time.Duration
}

type Server struct {
	log           *slog.Logger
	db            *store.DB
	box           *secrets.Box
	hasher        *auth.Hasher
	secureCookies bool
	trusted       []netip.Prefix
	setupFile     string
	agents        AgentHub
	agentURL      string
	now           func() time.Time
	imagesKept    int
	mailPorts     map[int]int
	mailMu        sync.Mutex // serializes changes to the mail server
	mailTLSMu     sync.Mutex
	mailTLS       mailTLSStatus // the last certificate sync
	mailCheckMu   sync.Mutex    // one deliverability check at a time
	// mailReloadPending: the mail server's last settings reload failed.
	mailReloadPending atomic.Bool
	mailCheckEvery    time.Duration
	catalog           func() ([]*catalog.Template, error)
	tplMu             sync.Mutex
	catalogMu         sync.Mutex
	remote            *remoteCatalog // the downloaded catalog
	catalogFetchMu    sync.Mutex     // one download at a time
	health            healthChecks
	tplUpdates        map[int64]*templateUpdate // running or failed template updates
	dataDir           string
	restic            string
	telegramAPI       string
	notifyPrivate     bool            // tests: chat services on private addresses
	notifyRetries     []time.Duration // pauses before delivery attempts
	notifySem         chan struct{}   // deliveries running at once
	offlineAfter      time.Duration

	setupMu    sync.Mutex
	setupToken string

	ipLimiter    *auth.Limiter // all credential endpoints, per client IP
	emailLimiter *auth.Limiter // password attempts, per account
	mfaLimiter   *auth.Limiter // second-factor attempts, per account
	hookLimiter  *auth.Limiter // webhook deliveries, per app

	live *liveLogs // output of running deployments
	// backupLive holds the output of running backups and restores.
	backupLive *liveLogs
	pendMu     sync.Mutex
	pending    map[int64]string // per app: commit pushed while a deployment ran
	streamMu   sync.Mutex
	streams    map[int64]int // open event streams per user
}

func New(o Options) *Server {
	if o.MailCheckEvery == 0 {
		o.MailCheckEvery = 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Restic == "" {
		o.Restic = "restic"
	}
	if o.ImagesKept <= 0 {
		o.ImagesKept = defaultImagesKept
	}
	notifyPrivate := o.TelegramAPI != ""
	if o.TelegramAPI == "" {
		o.TelegramAPI = telegramAPI
	}
	if len(o.NotifyRetries) == 0 {
		o.NotifyRetries = notifyRetries
	}
	if o.OfflineAfter <= 0 {
		o.OfflineAfter = 5 * time.Minute
	}
	return &Server{
		log:            o.Log,
		db:             o.DB,
		box:            o.Box,
		hasher:         o.Hasher,
		secureCookies:  o.SecureCookies,
		trusted:        o.TrustedProxies,
		setupFile:      o.SetupTokenFile,
		agents:         o.Agents,
		agentURL:       o.AgentURL,
		now:            o.Now,
		imagesKept:     o.ImagesKept,
		mailPorts:      o.MailHostPorts,
		mailCheckEvery: o.MailCheckEvery,
		catalog:        o.Catalog,
		tplUpdates:     map[int64]*templateUpdate{},
		dataDir:        o.DataDir,
		restic:         o.Restic,
		telegramAPI:    o.TelegramAPI,
		notifyPrivate:  notifyPrivate,
		notifyRetries:  o.NotifyRetries,
		notifySem:      make(chan struct{}, notifyWorkers),
		offlineAfter:   o.OfflineAfter,
		setupToken:     o.SetupToken,
		ipLimiter:      auth.NewLimiter(20, 3*time.Second).WithClock(o.Now),
		emailLimiter:   auth.NewLimiter(5, 30*time.Second).WithClock(o.Now),
		mfaLimiter:     auth.NewLimiter(5, 30*time.Second).WithClock(o.Now),
		hookLimiter:    auth.NewLimiter(20, 3*time.Second).WithClock(o.Now),
		live:           &liveLogs{m: map[int64]*liveLog{}},
		backupLive:     &liveLogs{m: map[int64]*liveLog{}},
		pending:        map[int64]string{},
		streams:        map[int64]int{},
	}
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)

	mux.HandleFunc("GET /api/v1/setup", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/setup", s.handleSetup)

	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.Handle("POST /api/v1/auth/mfa", s.requireMFAPending(s.handleMFA))
	mux.Handle("POST /api/v1/auth/logout", s.requireAnySession(s.handleLogout))
	mux.Handle("GET /api/v1/auth/me", s.requireRole(store.RoleViewer, s.handleMe))
	mux.Handle("POST /api/v1/auth/password", s.requireRole(store.RoleViewer, s.handleChangePassword))
	mux.Handle("POST /api/v1/auth/totp/setup", s.requireRole(store.RoleViewer, s.handleTOTPSetup))
	mux.Handle("POST /api/v1/auth/totp/enable", s.requireRole(store.RoleViewer, s.handleTOTPEnable))
	mux.Handle("POST /api/v1/auth/totp/disable", s.requireRole(store.RoleViewer, s.handleTOTPDisable))

	mux.Handle("GET /api/v1/users", s.requireRole(store.RoleAdmin, s.handleListUsers))
	mux.Handle("POST /api/v1/users", s.requireRole(store.RoleAdmin, s.handleCreateUser))

	mux.Handle("GET /api/v1/servers", s.requireRole(store.RoleViewer, s.handleListServers))
	mux.Handle("POST /api/v1/servers", s.requireRole(store.RoleAdmin, s.handleCreateServer))
	mux.Handle("GET /api/v1/servers/{id}", s.requireRole(store.RoleViewer, s.handleGetServer))
	mux.Handle("DELETE /api/v1/servers/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteServer))
	mux.Handle("GET /api/v1/servers/{id}/metrics", s.requireRole(store.RoleViewer, s.handleServerMetrics))
	mux.Handle("POST /api/v1/servers/{id}/ping", s.requireRole(store.RoleMember, s.handlePingServer))
	mux.Handle("PUT /api/v1/servers/{id}/public-ips", s.requireRole(store.RoleAdmin, s.handleSetPublicIPs))
	mux.Handle("POST /api/v1/servers/{id}/enroll-token", s.requireRole(store.RoleAdmin, s.handleResetEnrollToken))
	mux.Handle("GET /api/v1/servers/{id}/routes", s.requireRole(store.RoleViewer, s.handleListRoutes))
	mux.Handle("POST /api/v1/servers/{id}/routes", s.requireRole(store.RoleAdmin, s.handleCreateRoute))
	mux.Handle("DELETE /api/v1/servers/{id}/routes/{rid}", s.requireRole(store.RoleAdmin, s.handleDeleteRoute))

	mux.Handle("GET /api/v1/sites", s.requireRole(store.RoleViewer, s.handleListSites))
	mux.Handle("POST /api/v1/sites", s.requireRole(store.RoleAdmin, s.handleCreateSite))
	mux.Handle("GET /api/v1/sites/{id}", s.requireRole(store.RoleViewer, s.handleGetSite))
	mux.Handle("PUT /api/v1/sites/{id}/domains", s.requireRole(store.RoleAdmin, s.handleSetSiteDomains))
	mux.Handle("PUT /api/v1/sites/{id}/php", s.requireRole(store.RoleAdmin, s.handleSetSitePHP))
	mux.Handle("DELETE /api/v1/sites/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteSite))
	mux.Handle("GET /api/v1/sites/{id}/files", s.requireRole(store.RoleViewer, s.handleListFiles))
	mux.Handle("GET /api/v1/sites/{id}/files/stat", s.requireRole(store.RoleViewer, s.handleStatFile))
	mux.Handle("GET /api/v1/sites/{id}/files/text", s.requireRole(store.RoleViewer, s.handleReadText))
	mux.Handle("GET /api/v1/sites/{id}/files/content", s.requireRole(store.RoleViewer, s.handleDownload))
	mux.Handle("PUT /api/v1/sites/{id}/files/content", s.requireRole(store.RoleMember, s.handleUpload))
	mux.Handle("POST /api/v1/sites/{id}/files/{op}", s.requireRole(store.RoleMember, s.handleFileOp))
	mux.Handle("GET /api/v1/sites/{id}/databases", s.requireRole(store.RoleViewer, s.handleListDatabases))
	mux.Handle("POST /api/v1/sites/{id}/databases", s.requireRole(store.RoleAdmin, s.handleCreateDatabase))
	mux.Handle("POST /api/v1/sites/{id}/databases/{did}/password", s.requireRole(store.RoleAdmin, s.handleResetDatabasePassword))
	mux.Handle("DELETE /api/v1/sites/{id}/databases/{did}", s.requireRole(store.RoleAdmin, s.handleDeleteDatabase))
	s.backupRoutes(mux, "/api/v1/sites/{id}", s.pathSiteTarget)
	s.backupRoutes(mux, "/api/v1/apps/{id}", s.pathAppTarget)
	mux.Handle("GET /api/v1/panel-backup", s.requireRole(store.RoleAdmin, s.handlePanelBackup))
	mux.Handle("PUT /api/v1/panel-backup/recovery", s.requireRole(store.RoleOwner, s.handleSetRecovery))
	mux.Handle("PUT /api/v1/panel-backup/policy", s.requireRole(store.RoleAdmin, s.handleSetPanelBackupPolicy))
	mux.Handle("DELETE /api/v1/panel-backup/policy", s.requireRole(store.RoleAdmin, s.handleDeletePanelBackupPolicy))
	mux.Handle("POST /api/v1/panel-backup/run", s.requireRole(store.RoleAdmin, s.handleRunPanelBackup))
	mux.Handle("GET /api/v1/panel-backup/runs/{bid}", s.requireRole(store.RoleAdmin, s.handleGetPanelBackup))
	mux.Handle("GET /api/v1/backup-storages", s.requireRole(store.RoleAdmin, s.handleListStorages))
	mux.Handle("POST /api/v1/backup-storages", s.requireRole(store.RoleAdmin, s.handleCreateStorage))
	mux.Handle("PUT /api/v1/backup-storages/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateStorage))
	mux.Handle("DELETE /api/v1/backup-storages/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteStorage))
	mux.Handle("GET /api/v1/backup-storages/{id}/password", s.requireRole(store.RoleAdmin, s.handleStoragePassword))
	mux.Handle("POST /api/v1/backup-storages/{id}/check", s.requireRole(store.RoleAdmin, s.handleCheckStorage))
	mux.Handle("GET /api/v1/sites/{id}/sftp-users", s.requireRole(store.RoleViewer, s.handleListSFTPUsers))
	mux.Handle("POST /api/v1/sites/{id}/sftp-users", s.requireRole(store.RoleAdmin, s.handleCreateSFTPUser))
	mux.Handle("DELETE /api/v1/sites/{id}/sftp-users/{uid}", s.requireRole(store.RoleAdmin, s.handleDeleteSFTPUser))

	mux.Handle("GET /api/v1/dns", s.requireRole(store.RoleViewer, s.handleDNSOverview))
	mux.Handle("PUT /api/v1/dns/nameservers", s.requireRole(store.RoleAdmin, s.handleSetNameservers))
	mux.Handle("POST /api/v1/dns/zones", s.requireRole(store.RoleAdmin, s.handleCreateZone))
	mux.Handle("GET /api/v1/dns/zones/{id}", s.requireRole(store.RoleViewer, s.handleGetZone))
	mux.Handle("DELETE /api/v1/dns/zones/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteZone))
	mux.Handle("PUT /api/v1/dns/zones/{id}/dnssec", s.requireRole(store.RoleAdmin, s.handleSetDNSSEC))
	mux.Handle("POST /api/v1/dns/zones/{id}/records", s.requireRole(store.RoleAdmin, s.handlePutRecord))
	mux.Handle("PUT /api/v1/dns/zones/{id}/records/{rid}", s.requireRole(store.RoleAdmin, s.handlePutRecord))
	mux.Handle("DELETE /api/v1/dns/zones/{id}/records/{rid}", s.requireRole(store.RoleAdmin, s.handleDeleteRecord))

	mux.Handle("GET /api/v1/health-checks", s.requireRole(store.RoleViewer, s.handleHealthChecks))
	mux.Handle("POST /api/v1/health-checks", s.requireRole(store.RoleAdmin, s.handleCreateHealthCheck))
	mux.Handle("PUT /api/v1/health-checks/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateHealthCheck))
	mux.Handle("DELETE /api/v1/health-checks/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteHealthCheck))
	mux.Handle("GET /api/v1/health-checks/{id}/events", s.requireRole(store.RoleViewer, s.handleHealthEvents))
	mux.Handle("GET /api/v1/catalog", s.requireRole(store.RoleViewer, s.handleCatalog))
	mux.Handle("POST /api/v1/catalog/{id}/install", s.requireRole(store.RoleAdmin, s.handleInstall))
	mux.Handle("PUT /api/v1/catalog/source", s.requireRole(store.RoleAdmin, s.handleCatalogSource))
	mux.Handle("POST /api/v1/catalog/refresh", s.requireRole(store.RoleAdmin, s.handleCatalogRefresh))
	mux.Handle("GET /api/v1/apps/{id}/template", s.requireRole(store.RoleViewer, s.handleAppTemplate))
	mux.Handle("POST /api/v1/apps/{id}/template-update", s.requireRole(store.RoleAdmin, s.handleTemplateUpdate))
	mux.Handle("GET /api/v1/mail", s.requireRole(store.RoleViewer, s.handleMailOverview))
	mux.Handle("POST /api/v1/mail", s.requireRole(store.RoleAdmin, s.handleCreateMail))
	mux.Handle("PUT /api/v1/mail/settings", s.requireRole(store.RoleAdmin, s.handleMailSettings))
	mux.Handle("PUT /api/v1/mail/domains/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateMailDomain))
	mux.Handle("GET /api/v1/mail/domains/{id}/forwarders", s.requireRole(store.RoleViewer, s.handleListForwarders))
	mux.Handle("POST /api/v1/mail/domains/{id}/forwarders", s.requireRole(store.RoleAdmin, s.handleCreateForwarder))
	mux.Handle("PUT /api/v1/mail/domains/{id}/forwarders/{fid}", s.requireRole(store.RoleAdmin, s.handleUpdateForwarder))
	mux.Handle("DELETE /api/v1/mail/domains/{id}/forwarders/{fid}", s.requireRole(store.RoleAdmin, s.handleDeleteForwarder))
	mux.Handle("POST /api/v1/mail/checks", s.requireRole(store.RoleAdmin, s.handleMailChecks))
	mux.Handle("POST /api/v1/mail/domains", s.requireRole(store.RoleAdmin, s.handleCreateMailDomain))
	mux.Handle("DELETE /api/v1/mail/domains/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteMailDomain))
	mux.Handle("GET /api/v1/mail/domains/{id}/mailboxes", s.requireRole(store.RoleViewer, s.handleListMailboxes))
	mux.Handle("POST /api/v1/mail/domains/{id}/mailboxes", s.requireRole(store.RoleAdmin, s.handleCreateMailbox))
	mux.Handle("PUT /api/v1/mail/domains/{id}/mailboxes/{mid}", s.requireRole(store.RoleAdmin, s.handleUpdateMailbox))
	mux.Handle("DELETE /api/v1/mail/domains/{id}/mailboxes/{mid}", s.requireRole(store.RoleAdmin, s.handleDeleteMailbox))

	mux.Handle("GET /api/v1/registries", s.requireRole(store.RoleAdmin, s.handleListRegistries))
	mux.Handle("POST /api/v1/registries", s.requireRole(store.RoleAdmin, s.handleCreateRegistry))
	mux.Handle("PUT /api/v1/registries/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateRegistry))
	mux.Handle("DELETE /api/v1/registries/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteRegistry))

	mux.Handle("GET /api/v1/apps", s.requireRole(store.RoleViewer, s.handleListApps))
	mux.Handle("POST /api/v1/apps", s.requireRole(store.RoleAdmin, s.handleCreateApp))
	mux.Handle("GET /api/v1/apps/{id}", s.requireRole(store.RoleViewer, s.handleGetApp))
	mux.Handle("PUT /api/v1/apps/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateApp))
	mux.Handle("DELETE /api/v1/apps/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteApp))
	mux.Handle("GET /api/v1/notifications", s.requireRole(store.RoleAdmin, s.handleNotifications))
	mux.Handle("POST /api/v1/notifications", s.requireRole(store.RoleAdmin, s.handleCreateChannel))
	mux.Handle("PUT /api/v1/notifications/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateChannel))
	mux.Handle("DELETE /api/v1/notifications/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteChannel))
	mux.Handle("POST /api/v1/notifications/{id}/test", s.requireRole(store.RoleAdmin, s.handleTestChannel))
	mux.Handle("PUT /api/v1/notifications/panel-url", s.requireRole(store.RoleAdmin, s.handleSetPanelURL))
	mux.Handle("PUT /api/v1/apps/{id}/domains", s.requireRole(store.RoleAdmin, s.handleSetAppDomains))
	mux.Handle("POST /api/v1/apps/{id}/replicas", s.requireRole(store.RoleAdmin, s.handleAddReplica))
	mux.Handle("DELETE /api/v1/apps/{id}/replicas/{sid}", s.requireRole(store.RoleAdmin, s.handleRemoveReplica))
	mux.Handle("POST /api/v1/apps/{id}/deploy-key", s.requireRole(store.RoleAdmin, s.handleRegenerateDeployKey))
	mux.Handle("GET /api/v1/apps/{id}/webhook", s.requireRole(store.RoleAdmin, s.handleGetWebhook))
	mux.Handle("PUT /api/v1/apps/{id}/webhook", s.requireRole(store.RoleAdmin, s.handleSetWebhook))
	mux.Handle("GET /api/v1/database-engines", s.requireRole(store.RoleViewer, s.handleListEngines))
	mux.Handle("GET /api/v1/apps/{id}/connection", s.requireRole(store.RoleAdmin, s.handleDatabaseConnection))
	mux.Handle("POST /api/v1/apps/{id}/links", s.requireRole(store.RoleAdmin, s.handleLinkApp))
	mux.Handle("DELETE /api/v1/apps/{id}/links/{app}", s.requireRole(store.RoleAdmin, s.handleUnlinkApp))
	mux.Handle("GET /api/v1/apps/{id}/compose", s.requireRole(store.RoleAdmin, s.handleGetCompose))
	mux.Handle("PUT /api/v1/apps/{id}/compose", s.requireRole(store.RoleAdmin, s.handleSetCompose))
	// Authenticated by the delivery's signature, not a session.
	mux.HandleFunc("POST /api/v1/webhooks/apps/{id}", s.handleWebhook)
	mux.Handle("GET /api/v1/apps/{id}/env", s.requireRole(store.RoleAdmin, s.handleGetAppEnv))
	mux.Handle("PUT /api/v1/apps/{id}/env", s.requireRole(store.RoleAdmin, s.handleSetAppEnv))
	mux.Handle("POST /api/v1/apps/{id}/deploy", s.requireRole(store.RoleMember, s.handleDeployApp))
	mux.Handle("GET /api/v1/apps/{id}/deployments", s.requireRole(store.RoleViewer, s.handleListDeployments))
	mux.Handle("GET /api/v1/apps/{id}/deployments/{did}", s.requireRole(store.RoleViewer, s.handleGetDeployment))
	mux.Handle("GET /api/v1/apps/{id}/deployments/{did}/stream", s.requireRole(store.RoleViewer, s.handleDeploymentStream))
	mux.Handle("GET /api/v1/apps/{id}/metrics", s.requireRole(store.RoleViewer, s.handleAppMetrics))
	mux.Handle("GET /api/v1/apps/{id}/status", s.requireRole(store.RoleViewer, s.handleAppStatus))
	mux.Handle("GET /api/v1/apps/{id}/logs", s.requireRole(store.RoleMember, s.handleAppLogs))
	mux.Handle("GET /api/v1/apps/{id}/logs/stream", s.requireRole(store.RoleMember, s.handleAppLogsStream))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/", uiHandler())

	var h http.Handler = mux
	h = s.authenticate(h)
	// Rejects cross-origin state-changing browser requests (CSRF) using
	// Sec-Fetch-Site and Origin. Complements SameSite=Strict cookies.
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
	h = s.logRequests(h)
	h = realIP(s.trusted, h)
	h = s.recoverPanics(h)
	return h
}

// Serve listens on addr until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	go s.cleanupLoop(ctx)
	go s.backupLoop(ctx)
	go s.dnsLoop(ctx)
	go s.refreshMailLoop(ctx)
	go s.catalogLoop(ctx)
	go s.healthLoop(ctx)
	go s.WatchServers(ctx)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down http server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func (s *Server) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.db.DeleteExpiredSessions(ctx, s.now()); err != nil && ctx.Err() == nil {
			s.log.Error("delete expired sessions", "err", err)
		} else if n > 0 {
			s.log.Debug("deleted expired sessions", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
