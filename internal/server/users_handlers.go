package server

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/store"
)

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, p *principal) {
	users, err := s.db.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, r, "list users", err)
		return
	}
	out := make([]userView, len(users))
	for i, u := range users {
		out[i] = viewUser(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserRequest struct {
	Email    string     `json:"email"`
	Name     string     `json:"name"`
	Password string     `json:"password"`
	Role     store.Role `json:"role"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Role.Valid() {
		writeError(w, http.StatusUnprocessableEntity, "role must be one of owner, admin, member, viewer")
		return
	}
	// Nobody can grant more than they have; only owners create owners.
	if !p.user.Role.AtLeast(req.Role) {
		writeError(w, http.StatusForbidden, "cannot grant a role higher than your own")
		return
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	name, err := validateName(req.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash, err := s.hasher.Hash(r.Context(), req.Password)
	if err != nil {
		s.internalError(w, r, "hash password", err)
		return
	}
	u, err := s.db.CreateUser(r.Context(), email, name, hash, req.Role)
	if errors.Is(err, store.ErrEmailTaken) {
		writeError(w, http.StatusConflict, "a user with this email already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create user", err)
		return
	}
	s.audit(r, &p.user.ID, "user.created", map[string]any{"user_id": u.ID, "email": u.Email, "role": u.Role})
	writeJSON(w, http.StatusCreated, map[string]any{"user": viewUser(u)})
}

// targetUser loads the user an admin acts on; it refuses users who outrank
// the admin.
func (s *Server) targetUser(w http.ResponseWriter, r *http.Request, p *principal) (*store.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return nil, false
	}
	u, err := s.db.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "load user", err)
		return nil, false
	}
	if !p.user.Role.AtLeast(u.Role) {
		writeError(w, http.StatusForbidden, "only an owner can manage an owner")
		return nil, false
	}
	return u, true
}

type updateUserRequest struct {
	Name     *string     `json:"name"`
	Role     *store.Role `json:"role"`
	Disabled *bool       `json:"disabled"`
}

// handleUpdateUser renames a user, changes their role or disables them.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, p *principal) {
	u, ok := s.targetUser(w, r, p)
	if !ok {
		return
	}
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var c store.UserChange
	details := map[string]any{"user_id": u.ID, "email": u.Email}
	if req.Name != nil {
		name, err := validateName(*req.Name)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		c.Name, details["name"] = &name, name
	}
	if req.Role != nil && *req.Role != u.Role {
		if !req.Role.Valid() {
			writeError(w, http.StatusUnprocessableEntity, "role must be one of owner, admin, member, viewer")
			return
		}
		if !p.user.Role.AtLeast(*req.Role) {
			writeError(w, http.StatusForbidden, "cannot grant a role higher than your own")
			return
		}
		c.Role, details["role"] = req.Role, *req.Role
	}
	if req.Disabled != nil && *req.Disabled != u.Disabled {
		c.Disabled, details["disabled"] = req.Disabled, *req.Disabled
	}
	if u.ID == p.user.ID && (c.Role != nil || c.Disabled != nil) {
		writeError(w, http.StatusUnprocessableEntity, "you cannot change your own role or disable yourself")
		return
	}
	updated, err := s.db.UpdateUser(r.Context(), u.ID, c)
	if errors.Is(err, store.ErrLastOwner) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, "update user", err)
		return
	}
	s.audit(r, &p.user.ID, "user.updated", details)
	writeJSON(w, http.StatusOK, map[string]any{"user": viewUser(updated)})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, p *principal) {
	u, ok := s.targetUser(w, r, p)
	if !ok {
		return
	}
	if u.ID == p.user.ID {
		writeError(w, http.StatusUnprocessableEntity, "you cannot delete yourself")
		return
	}
	switch err := s.db.DeleteUser(r.Context(), u.ID); {
	case errors.Is(err, store.ErrLastOwner):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "delete user", err)
		return
	}
	s.audit(r, &p.user.ID, "user.deleted", map[string]any{"user_id": u.ID, "email": u.Email, "role": u.Role})
	w.WriteHeader(http.StatusNoContent)
}

type resetPasswordRequest struct {
	Password  string `json:"password"`
	ResetTOTP bool   `json:"reset_totp"` // also turn off two-factor (a lost phone)
}

// handleResetPassword sets another user's password (no email reset yet) and
// signs them out everywhere.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request, p *principal) {
	u, ok := s.targetUser(w, r, p)
	if !ok {
		return
	}
	if u.ID == p.user.ID {
		writeError(w, http.StatusUnprocessableEntity, "change your own password under Account")
		return
	}
	var req resetPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash, err := s.hasher.Hash(r.Context(), req.Password)
	if err != nil {
		s.internalError(w, r, "hash password", err)
		return
	}
	if err := s.db.UpdatePasswordHash(r.Context(), u.ID, hash); err != nil {
		s.internalError(w, r, "update password", err)
		return
	}
	if req.ResetTOTP {
		if err := s.db.DisableTOTP(r.Context(), u.ID); err != nil {
			s.internalError(w, r, "turn off two-factor", err)
			return
		}
	}
	if err := s.db.DeleteUserSessions(r.Context(), u.ID, nil); err != nil {
		s.internalError(w, r, "revoke sessions", err)
		return
	}
	s.audit(r, &p.user.ID, "user.password_reset", map[string]any{"user_id": u.ID, "email": u.Email, "reset_totp": req.ResetTOTP})
	w.WriteHeader(http.StatusNoContent)
}

type sessionView struct {
	ID         string    `json:"id"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	Current    bool      `json:"current"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (s *Server) handleMySessions(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.UserSessions(r.Context(), p.user.ID, s.now())
	if err != nil {
		s.internalError(w, r, "list sessions", err)
		return
	}
	out := make([]sessionView, len(list))
	for i, x := range list {
		out[i] = sessionView{ID: store.SessionID(x.TokenHash), IP: x.IP, UserAgent: x.UserAgent,
			Current: bytes.Equal(x.TokenHash, p.session.TokenHash), CreatedAt: x.CreatedAt, LastSeenAt: x.LastSeenAt, ExpiresAt: x.ExpiresAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleEndSession signs out one of the user's other sessions.
func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	if id == store.SessionID(p.session.TokenHash) {
		writeError(w, http.StatusUnprocessableEntity, "this is the current session: sign out instead")
		return
	}
	if err := s.db.DeleteUserSessionByID(r.Context(), p.user.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	} else if err != nil {
		s.internalError(w, r, "end session", err)
		return
	}
	s.audit(r, &p.user.ID, "session.ended", nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleEndOtherSessions signs out everywhere but here.
func (s *Server) handleEndOtherSessions(w http.ResponseWriter, r *http.Request, p *principal) {
	if err := s.db.DeleteUserSessions(r.Context(), p.user.ID, p.session.TokenHash); err != nil {
		s.internalError(w, r, "end sessions", err)
		return
	}
	s.audit(r, &p.user.ID, "session.ended_others", nil)
	w.WriteHeader(http.StatusNoContent)
}
