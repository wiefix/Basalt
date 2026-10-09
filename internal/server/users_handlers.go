package server

import (
	"errors"
	"net/http"

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
