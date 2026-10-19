package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/store"
)

const (
	// apiTokenPrefix marks Basalt tokens (secret scanners can find them).
	apiTokenPrefix = "bst_"
	// tokenTouchInterval: the last use is recorded at most this often.
	tokenTouchInterval = time.Minute
	maxTokenDays       = 3 * 365
	// Expired tokens stay listed (as expired) this long.
	expiredTokensKept = 30 * 24 * time.Hour
)

// bearerToken returns the API token of an Authorization: Bearer header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

// tokenPrincipal resolves an API token; store.ErrNotFound when it is
// unknown, expired or its user is disabled.
func (s *Server) tokenPrincipal(r *http.Request, raw string) (*principal, error) {
	if !strings.HasPrefix(raw, apiTokenPrefix) || len(raw) > 128 {
		return nil, store.ErrNotFound
	}
	now := s.now()
	t, err := s.db.APITokenByHash(r.Context(), auth.HashToken(raw), now)
	if err != nil {
		return nil, err
	}
	u, err := s.db.UserByID(r.Context(), t.UserID)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, store.ErrNotFound
	}
	// The token acts with the lower of its role and its user's (whose role
	// may have changed since).
	capped := *u
	if !t.Role.AtLeast(u.Role) {
		capped.Role = t.Role
	}
	if t.ProjectID != 0 {
		capped.Role = store.RoleUser // nothing outside its project (roleIn)
	}
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= tokenTouchInterval {
		if err := s.db.TouchAPIToken(r.Context(), t.ID, clientIP(r), now); err != nil {
			s.log.Warn("record API token use", "err", err)
		}
	}
	return &principal{user: &capped, token: t, userRole: u.Role}, nil
}

type tokenView struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Prefix     string      `json:"prefix"`
	Role       store.Role  `json:"role"`
	ProjectID  int64       `json:"project_id,omitempty"` // limited to this project
	User       *tokenOwner `json:"user,omitempty"`       // in the list of all tokens
	ExpiresAt  *time.Time  `json:"expires_at"`
	Expired    bool        `json:"expired"`
	LastUsedAt *time.Time  `json:"last_used_at"`
	LastUsedIP string      `json:"last_used_ip,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

type tokenOwner struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

func (s *Server) viewToken(t *store.APIToken, owner bool) tokenView {
	v := tokenView{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Role: t.Role, ProjectID: t.ProjectID, ExpiresAt: t.ExpiresAt,
		Expired: t.Expired(s.now()), LastUsedAt: t.LastUsedAt, LastUsedIP: t.LastUsedIP, CreatedAt: t.CreatedAt}
	if owner {
		v.User = &tokenOwner{ID: t.UserID, Email: t.UserEmail}
	}
	return v
}

func (s *Server) writeTokens(w http.ResponseWriter, r *http.Request, userID int64) {
	list, err := s.db.APITokens(r.Context(), userID)
	if err != nil {
		s.internalError(w, r, "list API tokens", err)
		return
	}
	out := make([]tokenView, len(list))
	for i, t := range list {
		out[i] = s.viewToken(t, userID == 0)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleMyTokens(w http.ResponseWriter, r *http.Request, p *principal) {
	s.writeTokens(w, r, p.user.ID)
}

func (s *Server) handleAllTokens(w http.ResponseWriter, r *http.Request, p *principal) {
	s.writeTokens(w, r, 0)
}

type createTokenRequest struct {
	Name        string     `json:"name"`
	Role        store.Role `json:"role"`
	ExpiresDays int        `json:"expires_days"` // 0: no expiry
	// ProjectID limits the token to one project (0: all the user may do).
	ProjectID int64  `json:"project_id"`
	Password  string `json:"password"`
}

func validTokenName(n string) bool {
	if n == "" || utf8.RuneCountInString(n) > 64 || strings.TrimSpace(n) != n {
		return false
	}
	for _, c := range n {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// handleCreateToken creates a token for the signed-in user; it needs the
// password, so a stolen session alone cannot leave a lasting credential.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case !validTokenName(req.Name):
		writeError(w, http.StatusUnprocessableEntity, "name: 1 to 64 characters, no surrounding spaces")
		return
	case !req.Role.AtLeast(store.RoleViewer):
		writeError(w, http.StatusUnprocessableEntity, "role must be viewer, member, admin or owner")
		return
	case req.ProjectID == 0 && !s.highestRole(r, p).AtLeast(req.Role):
		writeError(w, http.StatusUnprocessableEntity, "a token cannot do more than your own role ("+string(s.highestRole(r, p))+")")
		return
	case req.ExpiresDays < 0 || req.ExpiresDays > maxTokenDays:
		writeError(w, http.StatusUnprocessableEntity, "expires_days must be 0 (never) to "+strconv.Itoa(maxTokenDays))
		return
	}
	if req.ProjectID != 0 {
		role, err := s.roleIn(r.Context(), p, req.ProjectID)
		if err != nil {
			s.internalError(w, r, "load project roles", err)
			return
		}
		switch {
		case !role.AtLeast(store.RoleViewer):
			writeError(w, http.StatusUnprocessableEntity, "project not found")
			return
		case !role.AtLeast(req.Role):
			writeError(w, http.StatusUnprocessableEntity, "a token cannot do more than your role in the project ("+string(role)+")")
			return
		}
	}
	if !s.checkPassword(w, r, p, req.Password) {
		return
	}
	secret := apiTokenPrefix + auth.NewToken()
	t := &store.APIToken{UserID: p.user.ID, UserEmail: p.user.Email, Name: req.Name, Role: req.Role, ProjectID: req.ProjectID,
		Prefix: secret[:len(apiTokenPrefix)+6]}
	if req.ExpiresDays > 0 {
		exp := s.now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		t.ExpiresAt = &exp
	}
	switch err := s.db.CreateAPIToken(r.Context(), t, auth.HashToken(secret)); {
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "you already have a token with this name")
		return
	case errors.Is(err, store.ErrTooManyTokens):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "create API token", err)
		return
	}
	s.audit(r, &p.user.ID, "token.created", map[string]any{"token_id": t.ID, "name": t.Name, "role": t.Role,
		"expires_days": req.ExpiresDays, "token_project_id": req.ProjectID})
	writeJSON(w, http.StatusCreated, map[string]any{"token": s.viewToken(t, false), "secret": secret})
}

// highestRole is the most the user may do anywhere: their global role or
// their highest role in a project. A token's role caps both, so a token
// of a project admin with role admin is an admin in that project only.
func (s *Server) highestRole(r *http.Request, p *principal) store.Role {
	best := p.user.Role
	if roles, err := s.db.UserProjectRoles(r.Context(), p.user.ID); err == nil {
		for _, pr := range roles {
			if !best.AtLeast(pr) {
				best = pr
			}
		}
	}
	return best
}

func (s *Server) handleDeleteMyToken(w http.ResponseWriter, r *http.Request, p *principal) {
	s.revokeToken(w, r, p, true)
}

// handleRevokeToken lets admins revoke any token of a user who does not
// outrank them.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, p *principal) {
	s.revokeToken(w, r, p, false)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p *principal, own bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	t, err := s.db.APIToken(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && own && t.UserID != p.user.ID) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load API token", err)
		return
	}
	if !own && t.UserID != p.user.ID {
		u, err := s.db.UserByID(r.Context(), t.UserID)
		if err != nil {
			s.internalError(w, r, "load token owner", err)
			return
		}
		if !p.user.Role.AtLeast(u.Role) {
			writeError(w, http.StatusForbidden, "only an owner can revoke an owner's tokens")
			return
		}
	}
	if err := s.db.DeleteAPIToken(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "delete API token", err)
		return
	}
	s.audit(r, &p.user.ID, "token.revoked", map[string]any{"token_id": t.ID, "name": t.Name, "user_id": t.UserID})
	w.WriteHeader(http.StatusNoContent)
}
