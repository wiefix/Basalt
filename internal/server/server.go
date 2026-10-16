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
	// CertCheckEvery is how often certificates are checked (default 6 hours).
	CertCheckEvery time.Duration
	// AuditCheckEvery is how often the audit log's chain is verified
	// (default daily).
	AuditCheckEvery time.Duration
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
	certCheckEvery    time.Duration
	auditCheckEvery   time.Duration
	auditWatch        auditWatch

	setupMu    sync.Mutex
	setupToken string

	ipLimiter    *auth.Limiter // all credential endpoints, per client IP
	emailLimiter *auth.Limiter // password attempts, per account
	mfaLimiter   *auth.Limiter // second-factor attempts, per account
	hookLimiter  *auth.Limiter // webhook deliveries, per app

	live *liveLogs // output of running deployments
	// backupLive holds the output of running backups and restores.
	backupLive  *liveLogs
	checkLive   *liveLogs  // output of running integrity checks
	panelRepoMu sync.Mutex // one restic job on the panel's repository at a time
	pendMu      sync.Mutex
	pending     map[int64]string // per app: commit pushed while a deployment ran
	stopsMu     sync.Mutex
	stops       map[int64]*deployStop // running deployments, by ID
	streamMu    sync.Mutex
	streams     map[int64]int // open event streams per user
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
	if o.CertCheckEvery <= 0 {
		o.CertCheckEvery = 6 * time.Hour
	}
	if o.AuditCheckEvery <= 0 {
		o.AuditCheckEvery = 24 * time.Hour
	}
	if o.Box != nil && o.DB != nil {
		// The master key is not in the database: the chain cannot be
		// forged by whoever can only write to it.
		o.DB.SetAuditKey(o.Box.DeriveKey("audit-log"))
	}
	return &Server{
		log:             o.Log,
		db:              o.DB,
		box:             o.Box,
		hasher:          o.Hasher,
		secureCookies:   o.SecureCookies,
		trusted:         o.TrustedProxies,
		setupFile:       o.SetupTokenFile,
		agents:          o.Agents,
		agentURL:        o.AgentURL,
		now:             o.Now,
		imagesKept:      o.ImagesKept,
		mailPorts:       o.MailHostPorts,
		mailCheckEvery:  o.MailCheckEvery,
		catalog:         o.Catalog,
		tplUpdates:      map[int64]*templateUpdate{},
		dataDir:         o.DataDir,
		restic:          o.Restic,
		telegramAPI:     o.TelegramAPI,
		notifyPrivate:   notifyPrivate,
		notifyRetries:   o.NotifyRetries,
		notifySem:       make(chan struct{}, notifyWorkers),
		offlineAfter:    o.OfflineAfter,
		certCheckEvery:  o.CertCheckEvery,
		auditCheckEvery: o.AuditCheckEvery,
		setupToken:      o.SetupToken,
		ipLimiter:       auth.NewLimiter(20, 3*time.Second).WithClock(o.Now),
		emailLimiter:    auth.NewLimiter(5, 30*time.Second).WithClock(o.Now),
		mfaLimiter:      auth.NewLimiter(5, 30*time.Second).WithClock(o.Now),
		hookLimiter:     auth.NewLimiter(20, 3*time.Second).WithClock(o.Now),
		live:            &liveLogs{m: map[int64]*liveLog{}},
		backupLive:      &liveLogs{m: map[int64]*liveLog{}},
		checkLive:       &liveLogs{m: map[int64]*liveLog{}},
		pending:         map[int64]string{},
		stops:           map[int64]*deployStop{},
		streams:         map[int64]int{},
	}
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	rt := &router{mux: mux}
	rt.Handle("GET /api/v1/openapi.json", s.requireRole(store.RoleViewer, func(w http.ResponseWriter, r *http.Request, p *principal) {
		s.serveOpenAPI(w, rt.routes)
	}))
	rt.HandleFunc("GET /healthz", s.handleHealth)
	rt.HandleFunc("GET /api/v1/version", s.handleVersion)

	rt.HandleFunc("GET /api/v1/setup", s.handleSetupStatus)
	rt.HandleFunc("POST /api/v1/setup", s.handleSetup)

	rt.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	rt.Handle("POST /api/v1/auth/mfa", s.requireMFAPending(s.handleMFA))
	rt.Handle("POST /api/v1/auth/logout", s.requireAnySession(s.handleLogout))
	rt.Handle("GET /api/v1/auth/me", s.requireRole(store.RoleViewer, s.handleMe))
	rt.Handle("POST /api/v1/auth/password", s.requireSession(store.RoleViewer, s.handleChangePassword))
	rt.Handle("POST /api/v1/auth/totp/setup", s.requireSession(store.RoleViewer, s.handleTOTPSetup))
	rt.Handle("POST /api/v1/auth/totp/enable", s.requireSession(store.RoleViewer, s.handleTOTPEnable))
	rt.Handle("POST /api/v1/auth/totp/disable", s.requireSession(store.RoleViewer, s.handleTOTPDisable))

	rt.Handle("GET /api/v1/users", s.requireRole(store.RoleAdmin, s.handleListUsers))
	rt.Handle("PUT /api/v1/users/{id}", s.requireSession(store.RoleAdmin, s.handleUpdateUser))
	rt.Handle("DELETE /api/v1/users/{id}", s.requireSession(store.RoleAdmin, s.handleDeleteUser))
	rt.Handle("PUT /api/v1/users/{id}/password", s.requireSession(store.RoleAdmin, s.handleResetPassword))
	rt.Handle("GET /api/v1/account/sessions", s.requireSession(store.RoleViewer, s.handleMySessions))
	rt.Handle("DELETE /api/v1/account/sessions", s.requireSession(store.RoleViewer, s.handleEndOtherSessions))
	rt.Handle("DELETE /api/v1/account/sessions/{id}", s.requireSession(store.RoleViewer, s.handleEndSession))
	rt.Handle("GET /api/v1/account/tokens", s.requireSession(store.RoleViewer, s.handleMyTokens))
	rt.Handle("POST /api/v1/account/tokens", s.requireSession(store.RoleViewer, s.handleCreateToken))
	rt.Handle("DELETE /api/v1/account/tokens/{id}", s.requireSession(store.RoleViewer, s.handleDeleteMyToken))
	rt.Handle("GET /api/v1/tokens", s.requireSession(store.RoleAdmin, s.handleAllTokens))
	rt.Handle("DELETE /api/v1/tokens/{id}", s.requireSession(store.RoleAdmin, s.handleRevokeToken))
	rt.Handle("POST /api/v1/users", s.requireSession(store.RoleAdmin, s.handleCreateUser))

	rt.Handle("GET /api/v1/servers", s.requireRole(store.RoleViewer, s.handleListServers))
	rt.Handle("POST /api/v1/servers", s.requireRole(store.RoleAdmin, s.handleCreateServer))
	rt.Handle("GET /api/v1/servers/{id}", s.requireRole(store.RoleViewer, s.handleGetServer))
	rt.Handle("DELETE /api/v1/servers/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteServer))
	rt.Handle("GET /api/v1/servers/{id}/metrics", s.requireRole(store.RoleViewer, s.handleServerMetrics))
	rt.Handle("GET /api/v1/servers/{id}/certificates", s.requireRole(store.RoleViewer, s.handleServerCerts))
	rt.Handle("POST /api/v1/servers/{id}/ping", s.requireRole(store.RoleMember, s.handlePingServer))
	rt.Handle("PUT /api/v1/servers/{id}/public-ips", s.requireRole(store.RoleAdmin, s.handleSetPublicIPs))
	rt.Handle("POST /api/v1/servers/{id}/enroll-token", s.requireRole(store.RoleAdmin, s.handleResetEnrollToken))
	rt.Handle("GET /api/v1/servers/{id}/routes", s.requireRole(store.RoleViewer, s.handleListRoutes))
	rt.Handle("POST /api/v1/servers/{id}/routes", s.requireRole(store.RoleAdmin, s.handleCreateRoute))
	rt.Handle("DELETE /api/v1/servers/{id}/routes/{rid}", s.requireRole(store.RoleAdmin, s.handleDeleteRoute))

	rt.Handle("GET /api/v1/sites", s.requireRole(store.RoleViewer, s.handleListSites))
	rt.Handle("POST /api/v1/sites", s.requireRole(store.RoleAdmin, s.handleCreateSite))
	rt.Handle("GET /api/v1/sites/{id}", s.requireRole(store.RoleViewer, s.handleGetSite))
	rt.Handle("PUT /api/v1/sites/{id}/domains", s.requireRole(store.RoleAdmin, s.handleSetSiteDomains))
	rt.Handle("PUT /api/v1/sites/{id}/php", s.requireRole(store.RoleAdmin, s.handleSetSitePHP))
	rt.Handle("DELETE /api/v1/sites/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteSite))
	rt.Handle("GET /api/v1/sites/{id}/files", s.requireRole(store.RoleViewer, s.handleListFiles))
	rt.Handle("GET /api/v1/sites/{id}/files/stat", s.requireRole(store.RoleViewer, s.handleStatFile))
	rt.Handle("GET /api/v1/sites/{id}/files/text", s.requireRole(store.RoleViewer, s.handleReadText))
	rt.Handle("GET /api/v1/sites/{id}/files/content", s.requireRole(store.RoleViewer, s.handleDownload))
	rt.Handle("PUT /api/v1/sites/{id}/files/content", s.requireRole(store.RoleMember, s.handleUpload))
	rt.Handle("POST /api/v1/sites/{id}/files/{op}", s.requireRole(store.RoleMember, s.handleFileOp))
	rt.Handle("GET /api/v1/sites/{id}/databases", s.requireRole(store.RoleViewer, s.handleListDatabases))
	rt.Handle("POST /api/v1/sites/{id}/databases", s.requireRole(store.RoleAdmin, s.handleCreateDatabase))
	rt.Handle("POST /api/v1/sites/{id}/databases/{did}/password", s.requireRole(store.RoleAdmin, s.handleResetDatabasePassword))
	rt.Handle("DELETE /api/v1/sites/{id}/databases/{did}", s.requireRole(store.RoleAdmin, s.handleDeleteDatabase))
	s.backupRoutes(rt, "/api/v1/sites/{id}", s.pathSiteTarget)
	s.backupRoutes(rt, "/api/v1/apps/{id}", s.pathAppTarget)
	rt.Handle("GET /api/v1/panel-backup", s.requireRole(store.RoleAdmin, s.handlePanelBackup))
	rt.Handle("PUT /api/v1/panel-backup/recovery", s.requireSession(store.RoleOwner, s.handleSetRecovery))
	rt.Handle("PUT /api/v1/panel-backup/policy", s.requireRole(store.RoleAdmin, s.handleSetPanelBackupPolicy))
	rt.Handle("DELETE /api/v1/panel-backup/policy", s.requireRole(store.RoleAdmin, s.handleDeletePanelBackupPolicy))
	rt.Handle("POST /api/v1/panel-backup/run", s.requireRole(store.RoleAdmin, s.handleRunPanelBackup))
	rt.Handle("GET /api/v1/panel-backup/runs/{bid}", s.requireRole(store.RoleAdmin, s.handleGetPanelBackup))
	rt.Handle("GET /api/v1/backup-checks", s.requireRole(store.RoleAdmin, s.handleBackupChecks))
	rt.Handle("POST /api/v1/backup-checks", s.requireRole(store.RoleAdmin, s.handleStartCheck))
	rt.Handle("GET /api/v1/backup-checks/{cid}", s.requireRole(store.RoleAdmin, s.handleGetCheck))
	rt.Handle("GET /api/v1/backup-storages", s.requireRole(store.RoleAdmin, s.handleListStorages))
	rt.Handle("POST /api/v1/backup-storages", s.requireRole(store.RoleAdmin, s.handleCreateStorage))
	rt.Handle("PUT /api/v1/backup-storages/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateStorage))
	rt.Handle("DELETE /api/v1/backup-storages/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteStorage))
	rt.Handle("GET /api/v1/backup-storages/{id}/password", s.requireRole(store.RoleAdmin, s.handleStoragePassword))
	rt.Handle("POST /api/v1/backup-storages/{id}/check", s.requireRole(store.RoleAdmin, s.handleCheckStorage))
	rt.Handle("GET /api/v1/sites/{id}/sftp-users", s.requireRole(store.RoleViewer, s.handleListSFTPUsers))
	rt.Handle("POST /api/v1/sites/{id}/sftp-users", s.requireRole(store.RoleAdmin, s.handleCreateSFTPUser))
	rt.Handle("DELETE /api/v1/sites/{id}/sftp-users/{uid}", s.requireRole(store.RoleAdmin, s.handleDeleteSFTPUser))

	rt.Handle("GET /api/v1/dns", s.requireRole(store.RoleViewer, s.handleDNSOverview))
	rt.Handle("PUT /api/v1/dns/nameservers", s.requireRole(store.RoleAdmin, s.handleSetNameservers))
	rt.Handle("POST /api/v1/dns/zones", s.requireRole(store.RoleAdmin, s.handleCreateZone))
	rt.Handle("GET /api/v1/dns/zones/{id}", s.requireRole(store.RoleViewer, s.handleGetZone))
	rt.Handle("DELETE /api/v1/dns/zones/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteZone))
	rt.Handle("PUT /api/v1/dns/zones/{id}/dnssec", s.requireRole(store.RoleAdmin, s.handleSetDNSSEC))
	rt.Handle("POST /api/v1/dns/zones/{id}/records", s.requireRole(store.RoleAdmin, s.handlePutRecord))
	rt.Handle("PUT /api/v1/dns/zones/{id}/records/{rid}", s.requireRole(store.RoleAdmin, s.handlePutRecord))
	rt.Handle("DELETE /api/v1/dns/zones/{id}/records/{rid}", s.requireRole(store.RoleAdmin, s.handleDeleteRecord))

	rt.Handle("GET /api/v1/audit", s.requireRole(store.RoleAdmin, s.handleAudit))
	rt.Handle("GET /api/v1/audit/verify", s.requireRole(store.RoleAdmin, s.handleVerifyAudit))
	rt.Handle("GET /api/v1/health-checks", s.requireRole(store.RoleViewer, s.handleHealthChecks))
	rt.Handle("POST /api/v1/health-checks", s.requireRole(store.RoleAdmin, s.handleCreateHealthCheck))
	rt.Handle("PUT /api/v1/health-checks/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateHealthCheck))
	rt.Handle("DELETE /api/v1/health-checks/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteHealthCheck))
	rt.Handle("GET /api/v1/health-checks/{id}/events", s.requireRole(store.RoleViewer, s.handleHealthEvents))
	rt.Handle("GET /api/v1/catalog", s.requireRole(store.RoleViewer, s.handleCatalog))
	rt.Handle("POST /api/v1/catalog/{id}/install", s.requireRole(store.RoleAdmin, s.handleInstall))
	rt.Handle("PUT /api/v1/catalog/source", s.requireRole(store.RoleAdmin, s.handleCatalogSource))
	rt.Handle("POST /api/v1/catalog/refresh", s.requireRole(store.RoleAdmin, s.handleCatalogRefresh))
	rt.Handle("GET /api/v1/apps/{id}/template", s.requireRole(store.RoleViewer, s.handleAppTemplate))
	rt.Handle("POST /api/v1/apps/{id}/template-update", s.requireRole(store.RoleAdmin, s.handleTemplateUpdate))
	rt.Handle("GET /api/v1/mail", s.requireRole(store.RoleViewer, s.handleMailOverview))
	rt.Handle("POST /api/v1/mail", s.requireRole(store.RoleAdmin, s.handleCreateMail))
	rt.Handle("PUT /api/v1/mail/settings", s.requireRole(store.RoleAdmin, s.handleMailSettings))
	rt.Handle("PUT /api/v1/mail/domains/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateMailDomain))
	rt.Handle("GET /api/v1/mail/domains/{id}/forwarders", s.requireRole(store.RoleViewer, s.handleListForwarders))
	rt.Handle("POST /api/v1/mail/domains/{id}/forwarders", s.requireRole(store.RoleAdmin, s.handleCreateForwarder))
	rt.Handle("PUT /api/v1/mail/domains/{id}/forwarders/{fid}", s.requireRole(store.RoleAdmin, s.handleUpdateForwarder))
	rt.Handle("DELETE /api/v1/mail/domains/{id}/forwarders/{fid}", s.requireRole(store.RoleAdmin, s.handleDeleteForwarder))
	rt.Handle("POST /api/v1/mail/checks", s.requireRole(store.RoleAdmin, s.handleMailChecks))
	rt.Handle("POST /api/v1/mail/domains", s.requireRole(store.RoleAdmin, s.handleCreateMailDomain))
	rt.Handle("DELETE /api/v1/mail/domains/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteMailDomain))
	rt.Handle("GET /api/v1/mail/domains/{id}/mailboxes", s.requireRole(store.RoleViewer, s.handleListMailboxes))
	rt.Handle("POST /api/v1/mail/domains/{id}/mailboxes", s.requireRole(store.RoleAdmin, s.handleCreateMailbox))
	rt.Handle("PUT /api/v1/mail/domains/{id}/mailboxes/{mid}", s.requireRole(store.RoleAdmin, s.handleUpdateMailbox))
	rt.Handle("DELETE /api/v1/mail/domains/{id}/mailboxes/{mid}", s.requireRole(store.RoleAdmin, s.handleDeleteMailbox))

	rt.Handle("GET /api/v1/registries", s.requireRole(store.RoleAdmin, s.handleListRegistries))
	rt.Handle("POST /api/v1/registries", s.requireRole(store.RoleAdmin, s.handleCreateRegistry))
	rt.Handle("PUT /api/v1/registries/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateRegistry))
	rt.Handle("DELETE /api/v1/registries/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteRegistry))

	rt.Handle("GET /api/v1/apps", s.requireRole(store.RoleViewer, s.handleListApps))
	rt.Handle("POST /api/v1/apps", s.requireRole(store.RoleAdmin, s.handleCreateApp))
	rt.Handle("GET /api/v1/apps/{id}", s.requireRole(store.RoleViewer, s.handleGetApp))
	rt.Handle("PUT /api/v1/apps/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateApp))
	rt.Handle("DELETE /api/v1/apps/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteApp))
	rt.Handle("GET /api/v1/notifications", s.requireRole(store.RoleAdmin, s.handleNotifications))
	rt.Handle("POST /api/v1/notifications", s.requireRole(store.RoleAdmin, s.handleCreateChannel))
	rt.Handle("PUT /api/v1/notifications/{id}", s.requireRole(store.RoleAdmin, s.handleUpdateChannel))
	rt.Handle("DELETE /api/v1/notifications/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteChannel))
	rt.Handle("POST /api/v1/notifications/{id}/test", s.requireRole(store.RoleAdmin, s.handleTestChannel))
	rt.Handle("PUT /api/v1/notifications/panel-url", s.requireRole(store.RoleAdmin, s.handleSetPanelURL))
	rt.Handle("PUT /api/v1/apps/{id}/domains", s.requireRole(store.RoleAdmin, s.handleSetAppDomains))
	rt.Handle("POST /api/v1/apps/{id}/replicas", s.requireRole(store.RoleAdmin, s.handleAddReplica))
	rt.Handle("DELETE /api/v1/apps/{id}/replicas/{sid}", s.requireRole(store.RoleAdmin, s.handleRemoveReplica))
	rt.Handle("POST /api/v1/apps/{id}/deploy-key", s.requireRole(store.RoleAdmin, s.handleRegenerateDeployKey))
	rt.Handle("GET /api/v1/apps/{id}/webhook", s.requireRole(store.RoleAdmin, s.handleGetWebhook))
	rt.Handle("PUT /api/v1/apps/{id}/webhook", s.requireRole(store.RoleAdmin, s.handleSetWebhook))
	rt.Handle("GET /api/v1/database-engines", s.requireRole(store.RoleViewer, s.handleListEngines))
	rt.Handle("GET /api/v1/apps/{id}/connection", s.requireRole(store.RoleAdmin, s.handleDatabaseConnection))
	rt.Handle("POST /api/v1/apps/{id}/links", s.requireRole(store.RoleAdmin, s.handleLinkApp))
	rt.Handle("DELETE /api/v1/apps/{id}/links/{app}", s.requireRole(store.RoleAdmin, s.handleUnlinkApp))
	rt.Handle("GET /api/v1/apps/{id}/compose", s.requireRole(store.RoleAdmin, s.handleGetCompose))
	rt.Handle("PUT /api/v1/apps/{id}/compose", s.requireRole(store.RoleAdmin, s.handleSetCompose))
	// Authenticated by the delivery's signature, not a session.
	rt.HandleFunc("POST /api/v1/webhooks/apps/{id}", s.handleWebhook)
	rt.Handle("GET /api/v1/apps/{id}/env", s.requireRole(store.RoleAdmin, s.handleGetAppEnv))
	rt.Handle("PUT /api/v1/apps/{id}/env", s.requireRole(store.RoleAdmin, s.handleSetAppEnv))
	rt.Handle("POST /api/v1/apps/{id}/deploy", s.requireRole(store.RoleMember, s.handleDeployApp))
	rt.Handle("GET /api/v1/apps/{id}/deployments", s.requireRole(store.RoleViewer, s.handleListDeployments))
	rt.Handle("GET /api/v1/apps/{id}/deployments/{did}", s.requireRole(store.RoleViewer, s.handleGetDeployment))
	rt.Handle("POST /api/v1/apps/{id}/deployments/{did}/cancel", s.requireRole(store.RoleMember, s.handleCancelDeployment))
	rt.Handle("GET /api/v1/apps/{id}/deployments/{did}/stream", s.requireRole(store.RoleViewer, s.handleDeploymentStream))
	rt.Handle("GET /api/v1/apps/{id}/metrics", s.requireRole(store.RoleViewer, s.handleAppMetrics))
	rt.Handle("GET /api/v1/apps/{id}/status", s.requireRole(store.RoleViewer, s.handleAppStatus))
	rt.Handle("GET /api/v1/apps/{id}/logs", s.requireRole(store.RoleMember, s.handleAppLogs))
	rt.Handle("GET /api/v1/apps/{id}/logs/stream", s.requireRole(store.RoleMember, s.handleAppLogsStream))

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
	go s.WatchCertificates(ctx)
	go s.WatchAuditLog(ctx)

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
		if _, err := s.db.DeleteExpiredAPITokens(ctx, s.now().Add(-expiredTokensKept)); err != nil && ctx.Err() == nil {
			s.log.Error("delete expired API tokens", "err", err)
		}
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
