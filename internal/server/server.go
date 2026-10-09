// Package server implements the control plane HTTP API.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/auth"
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
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

type Server struct {
	log           *slog.Logger
	db            *store.DB
	box           *secrets.Box
	hasher        *auth.Hasher
	secureCookies bool
	agents        AgentHub
	agentURL      string
	now           func() time.Time

	setupMu    sync.Mutex
	setupToken string

	ipLimiter    *auth.Limiter // all credential endpoints, per client IP
	emailLimiter *auth.Limiter // password attempts, per account
	mfaLimiter   *auth.Limiter // second-factor attempts, per account
}

func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Server{
		log:           o.Log,
		db:            o.DB,
		box:           o.Box,
		hasher:        o.Hasher,
		secureCookies: o.SecureCookies,
		agents:        o.Agents,
		agentURL:      o.AgentURL,
		now:           o.Now,
		setupToken:    o.SetupToken,
		ipLimiter:     auth.NewLimiter(20, 3*time.Second).WithClock(o.Now),
		emailLimiter:  auth.NewLimiter(5, 30*time.Second).WithClock(o.Now),
		mfaLimiter:    auth.NewLimiter(5, 30*time.Second).WithClock(o.Now),
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
	mux.Handle("POST /api/v1/servers/{id}/enroll-token", s.requireRole(store.RoleAdmin, s.handleResetEnrollToken))
	mux.Handle("GET /api/v1/servers/{id}/routes", s.requireRole(store.RoleViewer, s.handleListRoutes))
	mux.Handle("POST /api/v1/servers/{id}/routes", s.requireRole(store.RoleAdmin, s.handleCreateRoute))
	mux.Handle("DELETE /api/v1/servers/{id}/routes/{rid}", s.requireRole(store.RoleAdmin, s.handleDeleteRoute))

	mux.Handle("GET /api/v1/sites", s.requireRole(store.RoleViewer, s.handleListSites))
	mux.Handle("POST /api/v1/sites", s.requireRole(store.RoleAdmin, s.handleCreateSite))
	mux.Handle("GET /api/v1/sites/{id}", s.requireRole(store.RoleViewer, s.handleGetSite))
	mux.Handle("PUT /api/v1/sites/{id}/domains", s.requireRole(store.RoleAdmin, s.handleSetSiteDomains))
	mux.Handle("DELETE /api/v1/sites/{id}", s.requireRole(store.RoleAdmin, s.handleDeleteSite))
	mux.Handle("GET /api/v1/sites/{id}/files", s.requireRole(store.RoleViewer, s.handleListFiles))
	mux.Handle("GET /api/v1/sites/{id}/files/stat", s.requireRole(store.RoleViewer, s.handleStatFile))
	mux.Handle("GET /api/v1/sites/{id}/files/text", s.requireRole(store.RoleViewer, s.handleReadText))
	mux.Handle("GET /api/v1/sites/{id}/files/content", s.requireRole(store.RoleViewer, s.handleDownload))
	mux.Handle("PUT /api/v1/sites/{id}/files/content", s.requireRole(store.RoleMember, s.handleUpload))
	mux.Handle("POST /api/v1/sites/{id}/files/{op}", s.requireRole(store.RoleMember, s.handleFileOp))
	mux.Handle("GET /api/v1/sites/{id}/sftp-users", s.requireRole(store.RoleViewer, s.handleListSFTPUsers))
	mux.Handle("POST /api/v1/sites/{id}/sftp-users", s.requireRole(store.RoleAdmin, s.handleCreateSFTPUser))
	mux.Handle("DELETE /api/v1/sites/{id}/sftp-users/{uid}", s.requireRole(store.RoleAdmin, s.handleDeleteSFTPUser))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})

	var h http.Handler = mux
	h = s.authenticate(h)
	// Rejects cross-origin state-changing browser requests (CSRF) using
	// Sec-Fetch-Site and Origin. Complements SameSite=Strict cookies.
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
	h = s.logRequests(h)
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
