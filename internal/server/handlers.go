package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.log.Error("health check: database unavailable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": buildinfo.Version,
		"commit":  buildinfo.Commit,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error(what, "err", err, "method", r.Method, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// audit records an action. Failures are logged, not returned: the action
// itself already happened.
func (s *Server) audit(r *http.Request, actorID *int64, action string, details map[string]any) {
	if err := s.db.Audit(r.Context(), actorID, action, clientIP(r), details); err != nil {
		s.log.Error("write audit log", "err", err, "action", action)
	}
}

type userView struct {
	ID          int64      `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        store.Role `json:"role"`
	TOTPEnabled bool       `json:"totp_enabled"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
}

func viewUser(u *store.User) userView {
	return userView{
		ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role,
		TOTPEnabled: u.TOTPEnabled(), Disabled: u.Disabled, CreatedAt: u.CreatedAt,
	}
}
