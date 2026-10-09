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

type principal struct {
	user    *store.User
	session *store.Session
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
	if p := principalFrom(r.Context()); p != nil {
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

// requireRole allows fully authenticated users with at least role min.
func (s *Server) requireRole(min store.Role, h principalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil || p.session.MFAPending {
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

// requireMFAPending allows only sessions waiting for the second factor.
func (s *Server) requireMFAPending(h principalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil || !p.session.MFAPending {
			writeError(w, http.StatusUnauthorized, "no pending two-factor login")
			return
		}
		h(w, r, p)
	})
}

func (s *Server) requireAnySession(h principalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		h(w, r, p)
	})
}
