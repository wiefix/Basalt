package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/store"
)

const (
	sessionIdleTimeout     = 12 * time.Hour
	sessionAbsoluteTimeout = 7 * 24 * time.Hour
	mfaPendingTimeout      = 5 * time.Minute
	// Avoid a database write on every request.
	sessionTouchInterval = time.Minute
)

// principal is who makes a request: a user signed in with a session, or
// an API token (session nil), whose user's role is then capped by the
// token's.
type principal struct {
	user    *store.User
	session *store.Session
	token   *store.APIToken
	// projects are the user's roles in the projects they belong to, loaded
	// on first use (roleIn).
	projects map[int64]store.Role
}

type principalKey struct{}

func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey{}).(*principal)
	return p
}

func (s *Server) cookieName() string {
	if s.secureCookies {
		// __Host- forces Secure, Path=/ and no Domain: the cookie cannot be
		// set or overwritten by a sibling subdomain.
		return "__Host-basalt_session"
	}
	return "basalt_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}

// startSession creates a new session with a fresh token and sets the cookie.
// Any session the request arrived with is deleted (prevents session fixation).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64, mfaPending bool) error {
	// A request with an API token has no session to replace.
	if p := principalFrom(r.Context()); p != nil && p.session != nil {
		if err := s.db.DeleteSession(r.Context(), p.session.TokenHash); err != nil {
			return err
		}
	}
	now := s.now()
	expires := now.Add(sessionIdleTimeout)
	if mfaPending {
		expires = now.Add(mfaPendingTimeout)
	}
	token := auth.NewToken()
	err := s.db.CreateSession(r.Context(), store.Session{
		TokenHash:  auth.HashToken(token),
		UserID:     userID,
		MFAPending: mfaPending,
		IP:         clientIP(r),
		UserAgent:  truncate(r.UserAgent(), 256),
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  expires,
	})
	if err != nil {
		return err
	}
	s.setSessionCookie(w, token, expires)
	return nil
}

// authenticate resolves the session cookie into a principal. It never rejects
// a request; the require* wrappers decide what is allowed.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw, ok := bearerToken(r); ok {
			p, err := s.tokenPrincipal(r, raw)
			if errors.Is(err, store.ErrNotFound) {
				// Tokens are unguessable; this only keeps a client stuck on
				// a revoked token from flooding the panel.
				if !s.ipLimiter.Allow("ip:" + clientIP(r)) {
					writeRateLimited(w, time.Minute)
					return
				}
				writeError(w, http.StatusUnauthorized, "invalid or expired API token")
				return
			}
			if err != nil {
				s.internalError(w, r, "load API token", err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
			return
		}
		c, err := r.Cookie(s.cookieName())
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.loadPrincipal(r.Context(), c.Value)
		if errors.Is(err, store.ErrNotFound) {
			s.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			s.internalError(w, r, "load session", err)
			return
		}
		s.maybeTouch(r.Context(), w, c.Value, p.session)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func (s *Server) loadPrincipal(ctx context.Context, token string) (*principal, error) {
	sess, err := s.db.SessionByTokenHash(ctx, auth.HashToken(token), s.now())
	if err != nil {
		return nil, err
	}
	u, err := s.db.UserByID(ctx, sess.UserID)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, store.ErrNotFound
	}
	return &principal{user: u, session: sess}, nil
}

// maybeTouch slides the idle expiry forward, capped by the absolute lifetime.
func (s *Server) maybeTouch(ctx context.Context, w http.ResponseWriter, token string, sess *store.Session) {
	now := s.now()
	if sess.MFAPending || now.Sub(sess.LastSeenAt) < sessionTouchInterval {
		return
	}
	expires := now.Add(sessionIdleTimeout)
	if limit := sess.CreatedAt.Add(sessionAbsoluteTimeout); expires.After(limit) {
		expires = limit
	}
	if err := s.db.TouchSession(ctx, sess.TokenHash, now, expires); err != nil {
		s.log.Warn("touch session", "err", err)
		return
	}
	s.setSessionCookie(w, token, expires)
}

type principalHandler func(http.ResponseWriter, *http.Request, *principal)

// access is who may call a route (for the API description).
type access struct {
	role        store.Role // "" when no role is required
	sessionOnly bool       // not with an API token
	mfaPending  bool       // only between the password and the second factor
	// project: role is needed in the project of the resource the route is
	// about (a site, app, DNS zone or health check), not on the panel.
	project bool
}

// guarded is a handler behind an access check; the router records its
// access.
type guarded struct {
	http.Handler
	access access
}

// requireRole allows fully authenticated users with at least role min.
func (s *Server) requireRole(min store.Role, h principalHandler) http.Handler {
	return guarded{s.roleCheck(min, h), access{role: min}}
}

func (s *Server) roleCheck(min store.Role, h principalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil || (p.session != nil && p.session.MFAPending) {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !p.user.Role.AtLeast(min) {
			writeError(w, http.StatusForbidden, "insufficient permissions")
			return
		}
		h(w, r, p)
	})
}

// projectMinKey holds, for a route guarded by requireProject, the role it
// needs in the project of its resource.
type projectMinKey struct{}

// requireProject allows any signed-in user; the handler's resolver (pathSite,
// pathApp, pathZone, pathHealthCheck) loads the resource and checks that the
// user has at least min in its project (checkProject). A global role counts
// in every project.
func (s *Server) requireProject(min store.Role, h principalHandler) http.Handler {
	return guarded{s.roleCheck(store.RoleUser, func(w http.ResponseWriter, r *http.Request, p *principal) {
		h(w, r.WithContext(context.WithValue(r.Context(), projectMinKey{}, min)), p)
	}), access{role: min, project: true}}
}

// roleIn is the principal's role in a project: the higher of its global
// role and its role there (an API token caps both).
func (s *Server) roleIn(ctx context.Context, p *principal, project int64) (store.Role, error) {
	if p.projects == nil {
		m, err := s.db.UserProjectRoles(ctx, p.user.ID)
		if err != nil {
			return "", err
		}
		p.projects = m
	}
	role := p.user.Role
	if pr, ok := p.projects[project]; ok {
		if p.token != nil && !p.token.Role.AtLeast(pr) {
			pr = p.token.Role
		}
		if !role.AtLeast(pr) {
			role = pr
		}
	}
	return role, nil
}

// checkProject enforces the role a requireProject route needs in project,
// the project of the resource it loaded: a resource the principal cannot
// see is not found (notFound), one it can see but not change is forbidden.
// Routes guarded by a global role pass. It writes the error response itself.
func (s *Server) checkProject(w http.ResponseWriter, r *http.Request, project int64, notFound string) bool {
	min, ok := r.Context().Value(projectMinKey{}).(store.Role)
	if !ok {
		return true
	}
	p := principalFrom(r.Context())
	role, err := s.roleIn(r.Context(), p, project)
	if err != nil {
		s.internalError(w, r, "load project roles", err)
		return false
	}
	return roleAllows(w, role, min, notFound)
}

// checkGlobal is checkProject for a resource of no project (the mail
// server): only the global role counts.
func (s *Server) checkGlobal(w http.ResponseWriter, r *http.Request, notFound string) bool {
	min, ok := r.Context().Value(projectMinKey{}).(store.Role)
	if !ok {
		return true
	}
	return roleAllows(w, principalFrom(r.Context()).user.Role, min, notFound)
}

func roleAllows(w http.ResponseWriter, role, min store.Role, notFound string) bool {
	switch {
	case !role.AtLeast(store.RoleViewer):
		writeError(w, http.StatusNotFound, notFound)
		return false
	case !role.AtLeast(min):
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return false
	}
	return true
}

// canIn reports whether the principal has at least min in project (for
// lists and for the project a new resource goes to).
func (s *Server) canIn(ctx context.Context, p *principal, project int64, min store.Role) (bool, error) {
	role, err := s.roleIn(ctx, p, project)
	return err == nil && role.AtLeast(min), err
}

// requireSession is requireRole for what an API token may not do: manage
// the account's credentials (password, 2FA, tokens) and users.
func (s *Server) requireSession(min store.Role, h principalHandler) http.Handler {
	return guarded{s.roleCheck(min, func(w http.ResponseWriter, r *http.Request, p *principal) {
		if p.token != nil {
			writeError(w, http.StatusForbidden, "not allowed with an API token; sign in to the panel")
			return
		}
		h(w, r, p)
	}), access{role: min, sessionOnly: true}}
}

// requireMFAPending allows only sessions waiting for the second factor.
func (s *Server) requireMFAPending(h principalHandler) http.Handler {
	return guarded{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil || p.session == nil || !p.session.MFAPending {
			writeError(w, http.StatusUnauthorized, "no pending two-factor login")
			return
		}
		h(w, r, p)
	}), access{mfaPending: true}}
}

func (s *Server) requireAnySession(h principalHandler) http.Handler {
	return guarded{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil || p.session == nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		h(w, r, p)
	}), access{sessionOnly: true}}
}
