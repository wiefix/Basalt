package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/store"
)

const totpIssuer = "Basalt"

// --- first-run setup ---

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.CountUsers(r.Context())
	if err != nil {
		s.internalError(w, r, "count users", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"required": n == 0})
}

type setupRequest struct {
	SetupToken string `json:"setup_token"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Password   string `json:"password"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimiter.Allow("ip:" + clientIP(r)) {
		writeRateLimited(w, time.Minute)
		return
	}
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	s.setupMu.Lock()
	token := s.setupToken
	s.setupMu.Unlock()
	if token == "" {
		writeError(w, http.StatusConflict, "setup already completed")
		return
	}
	// The setup token is printed in the server log at startup. Requiring it
	// stops whoever reaches a fresh install first from claiming it.
	if subtle.ConstantTimeCompare([]byte(req.SetupToken), []byte(token)) != 1 {
		s.audit(r, nil, "setup.failed", map[string]any{"reason": "bad setup token"})
		writeError(w, http.StatusForbidden, "invalid setup token")
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

	u, err := s.db.CreateFirstOwner(r.Context(), email, name, hash)
	if errors.Is(err, store.ErrSetupDone) {
		writeError(w, http.StatusConflict, "setup already completed")
		return
	}
	if err != nil {
		s.internalError(w, r, "create owner", err)
		return
	}

	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	if s.setupFile != "" {
		os.Remove(s.setupFile)
	}

	s.audit(r, &u.ID, "setup.completed", map[string]any{"email": u.Email})
	s.log.Info("setup completed", "owner", u.Email)
	if err := s.startSession(w, r, u.ID, false); err != nil {
		s.internalError(w, r, "start session", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": viewUser(u)})
}

// --- login ---

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.ipLimiter.Allow("ip:" + ip) {
		writeRateLimited(w, time.Minute)
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !s.emailLimiter.Allow("email:" + email) {
		s.audit(r, nil, "login.rate_limited", map[string]any{"email": truncate(email, 254)})
		writeRateLimited(w, time.Minute)
		return
	}

	u, err := s.db.UserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "load user", err)
		return
	}
	if u == nil || u.Disabled {
		s.hasher.VerifyDummy(r.Context(), req.Password)
		s.audit(r, nil, "login.failed", map[string]any{"email": truncate(email, 254), "reason": "unknown or disabled account"})
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	ok, rehash, err := s.hasher.Verify(r.Context(), u.PasswordHash, req.Password)
	if err != nil {
		s.internalError(w, r, "verify password", err)
		return
	}
	if !ok {
		s.audit(r, &u.ID, "login.failed", map[string]any{"reason": "bad password"})
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if rehash {
		s.upgradeHash(r.Context(), u.ID, req.Password)
	}

	mfa := u.TOTPEnabled()
	if err := s.startSession(w, r, u.ID, mfa); err != nil {
		s.internalError(w, r, "start session", err)
		return
	}
	if mfa {
		s.audit(r, &u.ID, "login.password_ok", nil)
		writeJSON(w, http.StatusOK, map[string]any{"mfa_required": true})
		return
	}
	s.audit(r, &u.ID, "login.succeeded", nil)
	writeJSON(w, http.StatusOK, map[string]any{"mfa_required": false, "user": viewUser(u)})
}

func (s *Server) upgradeHash(ctx context.Context, userID int64, pw string) {
	hash, err := s.hasher.Hash(ctx, pw)
	if err == nil {
		err = s.db.UpdatePasswordHash(ctx, userID, hash)
	}
	if err != nil {
		s.log.Warn("upgrade password hash", "err", err, "user_id", userID)
	}
}

type codeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleMFA(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.mfaLimiter.Allow(fmt.Sprint("user:", p.user.ID)) {
		writeRateLimited(w, time.Minute)
		return
	}
	var req codeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ok, recovery, err := s.verifySecondFactor(r.Context(), p.user, req.Code)
	if err != nil {
		s.internalError(w, r, "verify second factor", err)
		return
	}
	if !ok {
		s.audit(r, &p.user.ID, "login.mfa_failed", nil)
		writeError(w, http.StatusUnauthorized, "invalid code")
		return
	}
	// Issue a new token for the full session; the pending one is discarded.
	if err := s.startSession(w, r, p.user.ID, false); err != nil {
		s.internalError(w, r, "start session", err)
		return
	}
	s.audit(r, &p.user.ID, "login.succeeded", map[string]any{"recovery_code": recovery})

	resp := map[string]any{"user": viewUser(p.user)}
	if recovery {
		n, err := s.db.RemainingRecoveryCodes(r.Context(), p.user.ID)
		if err == nil {
			resp["recovery_codes_remaining"] = n
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// verifySecondFactor accepts either a TOTP code or an unused recovery code.
func (s *Server) verifySecondFactor(ctx context.Context, u *store.User, code string) (ok, recovery bool, err error) {
	if !u.TOTPEnabled() {
		return false, false, nil
	}
	if auth.LooksLikeRecoveryCode(code) {
		ok, err := s.db.UseRecoveryCode(ctx, u.ID, auth.HashToken(auth.NormalizeRecoveryCode(code)))
		return ok, ok, err
	}
	secret, err := s.box.Open(u.TOTPSecret, totpAD(u.ID))
	if err != nil {
		return false, false, err
	}
	step, ok := auth.ValidateTOTP(secret, code, s.now(), u.TOTPLastStep)
	if !ok {
		return false, false, nil
	}
	ok, err = s.db.AdvanceTOTPStep(ctx, u.ID, step)
	return ok, false, err
}

func totpAD(userID int64) []byte        { return fmt.Appendf(nil, "totp:user:%d", userID) }
func totpPendingAD(userID int64) []byte { return fmt.Appendf(nil, "totp-pending:user:%d", userID) }

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, p *principal) {
	if err := s.db.DeleteSession(r.Context(), p.session.TokenHash); err != nil {
		s.internalError(w, r, "delete session", err)
		return
	}
	s.clearSessionCookie(w)
	s.audit(r, &p.user.ID, "logout", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, p *principal) {
	writeJSON(w, http.StatusOK, map[string]any{"user": viewUser(p.user)})
}

// --- account security ---

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, p *principal) {
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.checkPassword(w, r, p, req.CurrentPassword) {
		return
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash, err := s.hasher.Hash(r.Context(), req.NewPassword)
	if err != nil {
		s.internalError(w, r, "hash password", err)
		return
	}
	if err := s.db.UpdatePasswordHash(r.Context(), p.user.ID, hash); err != nil {
		s.internalError(w, r, "update password", err)
		return
	}
	// Sign out every other device.
	if err := s.db.DeleteUserSessions(r.Context(), p.user.ID, p.session.TokenHash); err != nil {
		s.internalError(w, r, "revoke sessions", err)
		return
	}
	s.audit(r, &p.user.ID, "password.changed", nil)
	w.WriteHeader(http.StatusNoContent)
}

// checkPassword re-authenticates the user for sensitive operations. It writes
// the error response itself and returns false on failure.
func (s *Server) checkPassword(w http.ResponseWriter, r *http.Request, p *principal, pw string) bool {
	if !s.emailLimiter.Allow("email:" + strings.ToLower(p.user.Email)) {
		writeRateLimited(w, time.Minute)
		return false
	}
	ok, _, err := s.hasher.Verify(r.Context(), p.user.PasswordHash, pw)
	if err != nil {
		s.internalError(w, r, "verify password", err)
		return false
	}
	if !ok {
		s.audit(r, &p.user.ID, "reauth.failed", nil)
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return false
	}
	return true
}

type passwordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request, p *principal) {
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if p.user.TOTPEnabled() {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}
	if !s.checkPassword(w, r, p, req.Password) {
		return
	}
	secret := auth.NewTOTPSecret()
	if err := s.db.SetTOTPPending(r.Context(), p.user.ID, s.box.Seal(secret, totpPendingAD(p.user.ID))); err != nil {
		s.internalError(w, r, "store pending totp", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": auth.TOTPSecretString(secret),
		"uri":    auth.TOTPKeyURI(totpIssuer, p.user.Email, secret),
	})
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.mfaLimiter.Allow(fmt.Sprint("user:", p.user.ID)) {
		writeRateLimited(w, time.Minute)
		return
	}
	var req codeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if p.user.TOTPEnabled() {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}
	if p.user.TOTPPending == nil {
		writeError(w, http.StatusConflict, "call /api/v1/auth/totp/setup first")
		return
	}
	secret, err := s.box.Open(p.user.TOTPPending, totpPendingAD(p.user.ID))
	if err != nil {
		s.internalError(w, r, "decrypt pending totp", err)
		return
	}
	step, ok := auth.ValidateTOTP(secret, req.Code, s.now(), 0)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "invalid code")
		return
	}

	codes := auth.NewRecoveryCodes()
	hashes := make([][]byte, len(codes))
	for i, c := range codes {
		hashes[i] = auth.HashToken(auth.NormalizeRecoveryCode(c))
	}
	if err := s.db.EnableTOTP(r.Context(), p.user.ID, s.box.Seal(secret, totpAD(p.user.ID)), step, hashes); err != nil {
		s.internalError(w, r, "enable totp", err)
		return
	}
	// Other devices logged in with the password alone must re-authenticate.
	if err := s.db.DeleteUserSessions(r.Context(), p.user.ID, p.session.TokenHash); err != nil {
		s.internalError(w, r, "revoke sessions", err)
		return
	}
	s.audit(r, &p.user.ID, "totp.enabled", nil)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type disableTOTPRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request, p *principal) {
	var req disableTOTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !p.user.TOTPEnabled() {
		writeError(w, http.StatusConflict, "two-factor authentication is not enabled")
		return
	}
	if !s.checkPassword(w, r, p, req.Password) {
		return
	}
	if !s.mfaLimiter.Allow(fmt.Sprint("user:", p.user.ID)) {
		writeRateLimited(w, time.Minute)
		return
	}
	ok, _, err := s.verifySecondFactor(r.Context(), p.user, req.Code)
	if err != nil {
		s.internalError(w, r, "verify second factor", err)
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "invalid code")
		return
	}
	if err := s.db.DisableTOTP(r.Context(), p.user.ID); err != nil {
		s.internalError(w, r, "disable totp", err)
		return
	}
	s.audit(r, &p.user.ID, "totp.disabled", nil)
	w.WriteHeader(http.StatusNoContent)
}
