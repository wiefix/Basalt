package server

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wiefix/basalt/internal/store"
	"github.com/wiefix/basalt/internal/webauthn"
)

// Passkeys sign a user in without a password: the authenticator (phone,
// security key, password manager) verifies the user with a PIN or
// biometrics, so a passkey counts as both factors. A passkey belongs to the
// host name it was registered on (the WebAuthn RP ID): the panel takes it
// from the request, so it works behind the panel's own proxy and on
// localhost, and refuses requests whose Origin does not match their Host.

const (
	passkeyChallengeTTL = 5 * time.Minute
	maxChallenges       = 10000
	passkeyTimeoutMS    = int(passkeyChallengeTTL / time.Millisecond)
)

var b64url = base64.RawURLEncoding

// challenge is a WebAuthn challenge waiting for its answer.
type challenge struct {
	value   []byte
	rp      webauthn.RelyingParty
	expires time.Time
}

// challenges holds pending challenges, each usable once.
type challenges struct {
	mu sync.Mutex
	m  map[string]challenge
}

func (c *challenges) put(key string, ch challenge) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]challenge{}
	}
	if len(c.m) >= maxChallenges {
		now := time.Now()
		for k, v := range c.m {
			if now.After(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxChallenges {
			return false
		}
	}
	c.m[key] = ch
	return true
}

// take removes and returns the challenge under key if it has not expired.
func (c *challenges) take(key string, now time.Time) (challenge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.m[key]
	delete(c.m, key)
	if !ok || now.After(ch.expires) {
		return challenge{}, false
	}
	return ch, true
}

// relyingParty is the WebAuthn site of a request: its host name, and the
// origin the browser reports, which must name the same host.
func relyingParty(r *http.Request) (webauthn.RelyingParty, error) {
	origin := r.Header.Get("Origin")
	u, err := url.Parse(origin)
	if origin == "" || err != nil || u.Host == "" || u.Path != "" {
		return webauthn.RelyingParty{}, errors.New("the request has no valid Origin header")
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	if strings.ToLower(u.Hostname()) != host {
		return webauthn.RelyingParty{}, errors.New("the Origin header does not match the panel's address")
	}
	if net.ParseIP(host) != nil {
		return webauthn.RelyingParty{}, errors.New("passkeys need the panel to be opened by its domain name, not an IP address")
	}
	// Browsers treat localhost and *.localhost as secure without TLS.
	if u.Scheme != "https" && host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return webauthn.RelyingParty{}, errors.New("passkeys need the panel to be opened over HTTPS")
	}
	return webauthn.RelyingParty{ID: host, Origin: origin}, nil
}

type passkeyView struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Host       string     `json:"host"`   // the RP ID it works on
	Synced     bool       `json:"synced"` // backed up by its password manager
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func viewPasskey(p *store.Passkey) passkeyView {
	return passkeyView{ID: p.ID, Name: p.Name, Host: p.RPID, Synced: p.BackedUp, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt}
}

func (s *Server) handleMyPasskeys(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.Passkeys(r.Context(), p.user.ID)
	if err != nil {
		s.internalError(w, r, "list passkeys", err)
		return
	}
	out := make([]passkeyView, len(list))
	for i, k := range list {
		out[i] = viewPasskey(k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out})
}

// handlePasskeyOptions starts adding a passkey: after the password, it
// returns what navigator.credentials.create needs.
func (s *Server) handlePasskeyOptions(w http.ResponseWriter, r *http.Request, p *principal) {
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rp, err := relyingParty(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !s.checkPassword(w, r, p, req.Password) {
		return
	}
	existing, err := s.db.Passkeys(r.Context(), p.user.ID)
	if err != nil {
		s.internalError(w, r, "list passkeys", err)
		return
	}
	if len(existing) >= store.MaxPasskeys {
		writeError(w, http.StatusConflict, store.ErrTooManyPasskeys.Error())
		return
	}
	handle, err := s.db.WebAuthnHandle(r.Context(), p.user.ID)
	if err != nil {
		s.internalError(w, r, "passkey handle", err)
		return
	}
	value := webauthn.NewChallenge()
	if !s.challenges.put("reg:"+store.SessionID(p.session.TokenHash), challenge{value: value, rp: rp, expires: s.now().Add(passkeyChallengeTTL)}) {
		writeError(w, http.StatusServiceUnavailable, "too many sign-ins in progress, try again shortly")
		return
	}
	params := []map[string]any{}
	for _, alg := range webauthn.Algorithms {
		params = append(params, map[string]any{"type": "public-key", "alg": alg})
	}
	exclude := []map[string]any{}
	for _, k := range existing {
		exclude = append(exclude, map[string]any{"type": "public-key", "id": b64url.EncodeToString(k.CredentialID)})
	}
	// The JSON form of PublicKeyCredentialCreationOptions (binary fields in
	// base64url).
	writeJSON(w, http.StatusOK, map[string]any{"options": map[string]any{
		"rp":                     map[string]any{"id": rp.ID, "name": "Basalt"},
		"user":                   map[string]any{"id": b64url.EncodeToString(handle), "name": p.user.Email, "displayName": p.user.Name},
		"challenge":              b64url.EncodeToString(value),
		"pubKeyCredParams":       params,
		"excludeCredentials":     exclude,
		"authenticatorSelection": map[string]any{"residentKey": "required", "requireResidentKey": true, "userVerification": "required"},
		"attestation":            "none",
		"timeout":                passkeyTimeoutMS,
	}})
}

type addPasskeyRequest struct {
	Name              string `json:"name"`
	ClientDataJSON    string `json:"client_data_json"`   // base64url
	AttestationObject string `json:"attestation_object"` // base64url
}

func decodeB64(s string) ([]byte, error) { return b64url.DecodeString(strings.TrimRight(s, "=")) }

func validPasskeyName(n string) bool {
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

func (s *Server) handleAddPasskey(w http.ResponseWriter, r *http.Request, p *principal) {
	var req addPasskeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validPasskeyName(req.Name) {
		writeError(w, http.StatusUnprocessableEntity, "name: 1 to 64 characters, no surrounding spaces")
		return
	}
	cd, err1 := decodeB64(req.ClientDataJSON)
	att, err2 := decodeB64(req.AttestationObject)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "client_data_json and attestation_object must be base64url")
		return
	}
	ch, ok := s.challenges.take("reg:"+store.SessionID(p.session.TokenHash), s.now())
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "the passkey request expired: start again")
		return
	}
	if rp, err := relyingParty(r); err != nil || rp != ch.rp {
		writeError(w, http.StatusUnprocessableEntity, "the passkey was created for another address of the panel")
		return
	}
	cred, err := ch.rp.ParseRegistration(ch.value, cd, att)
	if err != nil {
		s.audit(r, &p.user.ID, "passkey.add_failed", map[string]any{"reason": truncate(err.Error(), 200)})
		writeError(w, http.StatusUnprocessableEntity, "the passkey was not accepted: "+err.Error())
		return
	}
	k := &store.Passkey{UserID: p.user.ID, CredentialID: cred.ID, PublicKey: cred.PublicKey, Alg: cred.Alg, SignCount: cred.SignCount,
		RPID: ch.rp.ID, Name: req.Name, AAGUID: cred.AAGUID, BackupEligible: cred.BackupEligible, BackedUp: cred.BackedUp}
	switch err := s.db.CreatePasskey(r.Context(), k); {
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "you already have a passkey with this name")
		return
	case errors.Is(err, store.ErrPasskeyExists), errors.Is(err, store.ErrTooManyPasskeys):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "store passkey", err)
		return
	}
	s.audit(r, &p.user.ID, "passkey.added", map[string]any{"passkey_id": k.ID, "name": k.Name, "host": k.RPID})
	writeJSON(w, http.StatusCreated, map[string]any{"passkey": viewPasskey(k)})
}

type renamePasskeyRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleRenamePasskey(w http.ResponseWriter, r *http.Request, p *principal) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req renamePasskeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validPasskeyName(req.Name) {
		writeError(w, http.StatusUnprocessableEntity, "name: 1 to 64 characters, no surrounding spaces")
		return
	}
	switch err := s.db.RenamePasskey(r.Context(), p.user.ID, id, req.Name); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "passkey not found")
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "you already have a passkey with this name")
		return
	case err != nil:
		s.internalError(w, r, "rename passkey", err)
		return
	}
	s.audit(r, &p.user.ID, "passkey.renamed", map[string]any{"passkey_id": id, "name": req.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeletePasskey(w http.ResponseWriter, r *http.Request, p *principal) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.DeletePasskey(r.Context(), p.user.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "passkey not found")
		return
	} else if err != nil {
		s.internalError(w, r, "delete passkey", err)
		return
	}
	s.audit(r, &p.user.ID, "passkey.removed", map[string]any{"passkey_id": id})
	w.WriteHeader(http.StatusNoContent)
}

// handlePasskeyLoginOptions starts a sign-in with a passkey: any passkey of
// this host (the browser lets the user pick one).
func (s *Server) handlePasskeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimiter.Allow("ip:" + clientIP(r)) {
		writeRateLimited(w, time.Minute)
		return
	}
	rp, err := relyingParty(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	value := webauthn.NewChallenge()
	id := b64url.EncodeToString(webauthn.NewChallenge())
	if !s.challenges.put("login:"+id, challenge{value: value, rp: rp, expires: s.now().Add(passkeyChallengeTTL)}) {
		writeError(w, http.StatusServiceUnavailable, "too many sign-ins in progress, try again shortly")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenge_id": id, "options": map[string]any{
		"challenge": b64url.EncodeToString(value), "rpId": rp.ID, "userVerification": "required",
		"allowCredentials": []any{}, "timeout": passkeyTimeoutMS,
	}})
}

type passkeyLoginRequest struct {
	ChallengeID       string `json:"challenge_id"`
	CredentialID      string `json:"credential_id"`      // base64url
	ClientDataJSON    string `json:"client_data_json"`   // base64url
	AuthenticatorData string `json:"authenticator_data"` // base64url
	Signature         string `json:"signature"`          // base64url
	UserHandle        string `json:"user_handle"`        // base64url, may be empty
}

// handlePasskeyLogin needs no rate limit of its own: each attempt uses up a
// challenge, and handing those out is limited.
func (s *Server) handlePasskeyLogin(w http.ResponseWriter, r *http.Request) {
	var req passkeyLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	credID, e1 := decodeB64(req.CredentialID)
	cd, e2 := decodeB64(req.ClientDataJSON)
	ad, e3 := decodeB64(req.AuthenticatorData)
	sig, e4 := decodeB64(req.Signature)
	handle, e5 := decodeB64(req.UserHandle)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || len(credID) == 0 {
		writeError(w, http.StatusBadRequest, "the passkey fields must be base64url")
		return
	}
	ch, ok := s.challenges.take("login:"+req.ChallengeID, s.now())
	if !ok {
		writeError(w, http.StatusUnauthorized, "the sign-in request expired: start again")
		return
	}
	if rp, err := relyingParty(r); err != nil || rp != ch.rp {
		writeError(w, http.StatusUnauthorized, "the sign-in was started on another address of the panel")
		return
	}
	fail := func(actor *int64, reason string) {
		s.audit(r, actor, "login.failed", map[string]any{"method": "passkey", "reason": reason})
		writeError(w, http.StatusUnauthorized, "this passkey cannot sign in here")
	}
	k, err := s.db.PasskeyByCredentialID(r.Context(), credID)
	if errors.Is(err, store.ErrNotFound) {
		fail(nil, "unknown passkey")
		return
	} else if err != nil {
		s.internalError(w, r, "load passkey", err)
		return
	}
	u, err := s.db.UserByID(r.Context(), k.UserID)
	if err != nil {
		s.internalError(w, r, "load user", err)
		return
	}
	if u.Disabled {
		fail(&u.ID, "disabled account")
		return
	}
	if k.RPID != ch.rp.ID {
		fail(&u.ID, "passkey of another host name ("+k.RPID+")")
		return
	}
	if len(handle) > 0 {
		own, err := s.db.WebAuthnHandle(r.Context(), u.ID)
		if err != nil || !bytes.Equal(own, handle) {
			fail(&u.ID, "user handle does not match")
			return
		}
	}
	got, err := ch.rp.VerifyAssertion(ch.value, &webauthn.Credential{PublicKey: k.PublicKey, SignCount: k.SignCount}, cd, ad, sig)
	if err != nil {
		fail(&u.ID, truncate(err.Error(), 200))
		return
	}
	if err := s.db.PasskeyUsed(r.Context(), k.ID, got.SignCount, got.BackedUp, s.now()); err != nil {
		s.internalError(w, r, "record passkey use", err)
		return
	}
	if err := s.startSession(w, r, u.ID, false); err != nil {
		s.internalError(w, r, "start session", err)
		return
	}
	s.audit(r, &u.ID, "login.succeeded", map[string]any{"method": "passkey", "passkey": k.Name})
	writeJSON(w, http.StatusOK, map[string]any{"user": viewUser(u)})
}
