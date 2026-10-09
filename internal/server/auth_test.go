package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/auth"
)

const (
	ownerEmail = "owner@example.com"
	ownerPass  = "correct horse battery staple"
)

// testClock is a manually advanced clock shared by server and test.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	clock *testClock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, srv: srv, ts: ts, clock: clock}
}

// client is a browser-like client with its own cookie jar.
type client struct {
	e  *env
	hc *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, hc: &http.Client{Jar: jar}}
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
}

func (c *client) do(method, path string, body any) resp {
	c.e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	r, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer r.Body.Close()
	out := resp{status: r.StatusCode, header: r.Header}
	_ = json.NewDecoder(r.Body).Decode(&out.body)
	return out
}

func (c *client) want(status int, method, path string, body any) resp {
	c.e.t.Helper()
	r := c.do(method, path, body)
	if r.status != status {
		c.e.t.Fatalf("%s %s: status %d, body %v; want %d", method, path, r.status, r.body, status)
	}
	return r
}

func (e *env) setupOwner() *client {
	c := e.client()
	c.want(http.StatusCreated, "POST", "/api/v1/setup", map[string]string{
		"setup_token": testSetupToken, "email": ownerEmail, "name": "Owner", "password": ownerPass,
	})
	return c
}

func (c *client) login(email, pass string) resp {
	return c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": pass})
}

func TestSetupRequiresTokenAndRunsOnce(t *testing.T) {
	e := newEnv(t)
	c := e.client()

	if r := c.want(200, "GET", "/api/v1/setup", nil); r.body["required"] != true {
		t.Fatalf("setup required = %v; want true", r.body["required"])
	}
	c.want(http.StatusForbidden, "POST", "/api/v1/setup", map[string]string{
		"setup_token": "wrong", "email": ownerEmail, "name": "Owner", "password": ownerPass,
	})
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/setup", map[string]string{
		"setup_token": testSetupToken, "email": ownerEmail, "name": "Owner", "password": "short",
	})
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/setup", map[string]string{
		"setup_token": testSetupToken, "email": "Name <x@y.z>", "name": "Owner", "password": ownerPass,
	})

	e.setupOwner()
	if r := c.want(200, "GET", "/api/v1/setup", nil); r.body["required"] != false {
		t.Fatal("setup still reported as required")
	}
	c.want(http.StatusConflict, "POST", "/api/v1/setup", map[string]string{
		"setup_token": testSetupToken, "email": "x@example.com", "name": "X", "password": ownerPass,
	})
}

func TestSetupLogsInOwner(t *testing.T) {
	c := newEnv(t).setupOwner()
	r := c.want(200, "GET", "/api/v1/auth/me", nil)
	user := r.body["user"].(map[string]any)
	if user["email"] != ownerEmail || user["role"] != "owner" {
		t.Fatalf("me = %v", user)
	}
}

func TestLoginLogout(t *testing.T) {
	e := newEnv(t)
	e.setupOwner()
	c := e.client()

	c.want(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
	if r := c.login(ownerEmail, "wrong password!!"); r.status != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", r.status)
	}
	if r := c.login("nobody@example.com", ownerPass); r.status != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", r.status)
	}
	// Email lookup is case-insensitive.
	if r := c.login("OWNER@example.com", ownerPass); r.status != 200 || r.body["mfa_required"] != false {
		t.Fatalf("login: %d %v", r.status, r.body)
	}
	c.want(200, "GET", "/api/v1/auth/me", nil)
	c.want(http.StatusNoContent, "POST", "/api/v1/auth/logout", nil)
	c.want(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
}

func TestSessionTokenRotatesAndIsStoredHashed(t *testing.T) {
	e := newEnv(t)
	c := e.setupOwner()
	u := mustURL(e.ts.URL)
	before := c.hc.Jar.Cookies(u)[0].Value

	if r := c.login(ownerEmail, ownerPass); r.status != 200 {
		t.Fatal(r.status)
	}
	after := c.hc.Jar.Cookies(u)[0].Value
	if before == after {
		t.Fatal("session token not rotated on login")
	}

	var n int
	e.srv.db.R.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, after).Scan(&n)
	if n != 0 {
		t.Fatal("raw session token found in database")
	}
	e.srv.db.R.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d sessions; old session should have been deleted on login", n)
	}
}

func TestSessionExpires(t *testing.T) {
	e := newEnv(t)
	c := e.setupOwner()
	e.clock.Advance(sessionIdleTimeout + time.Second)
	c.want(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
}

func TestTOTPFlow(t *testing.T) {
	e := newEnv(t)
	c := e.setupOwner()

	c.want(http.StatusForbidden, "POST", "/api/v1/auth/totp/setup", map[string]string{"password": "wrong password!!"})
	r := c.want(200, "POST", "/api/v1/auth/totp/setup", map[string]string{"password": ownerPass})
	if !strings.HasPrefix(r.body["uri"].(string), "otpauth://totp/Basalt:") {
		t.Fatalf("uri = %v", r.body["uri"])
	}
	secret, err := auth.DecodeTOTPSecret(r.body["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}

	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/auth/totp/enable", map[string]string{"code": "000000"})
	r = c.want(200, "POST", "/api/v1/auth/totp/enable", map[string]string{"code": auth.TOTPCodeAt(secret, e.clock.Now())})
	recovery := r.body["recovery_codes"].([]any)
	if len(recovery) != auth.RecoveryCodeCount {
		t.Fatalf("%d recovery codes", len(recovery))
	}
	// Enabling 2FA keeps the current session.
	c.want(200, "GET", "/api/v1/auth/me", nil)

	// Fresh login now needs the second factor.
	e.clock.Advance(30 * time.Second)
	c2 := e.client()
	if r := c2.login(ownerEmail, ownerPass); r.status != 200 || r.body["mfa_required"] != true {
		t.Fatalf("login: %d %v", r.status, r.body)
	}
	c2.want(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil) // pending session is not enough
	c2.want(http.StatusUnauthorized, "POST", "/api/v1/auth/mfa", map[string]string{"code": "123456"})
	code := auth.TOTPCodeAt(secret, e.clock.Now())
	c2.want(200, "POST", "/api/v1/auth/mfa", map[string]string{"code": code})
	c2.want(200, "GET", "/api/v1/auth/me", nil)
	c2.want(http.StatusUnauthorized, "POST", "/api/v1/auth/mfa", map[string]string{"code": code}) // session no longer pending

	// The same TOTP code cannot be replayed.
	c3 := e.client()
	c3.login(ownerEmail, ownerPass)
	c3.want(http.StatusUnauthorized, "POST", "/api/v1/auth/mfa", map[string]string{"code": code})

	// A recovery code works exactly once.
	e.clock.Advance(time.Minute) // refill the per-account second-factor rate limit
	rc := strings.ToUpper(recovery[0].(string))
	r = c3.want(200, "POST", "/api/v1/auth/mfa", map[string]string{"code": rc})
	if r.body["recovery_codes_remaining"] != float64(auth.RecoveryCodeCount-1) {
		t.Fatalf("remaining = %v", r.body["recovery_codes_remaining"])
	}
	c4 := e.client()
	c4.login(ownerEmail, ownerPass)
	c4.want(http.StatusUnauthorized, "POST", "/api/v1/auth/mfa", map[string]string{"code": rc})

	// Disabling requires password and a valid code.
	e.clock.Advance(30 * time.Second)
	c2.want(http.StatusForbidden, "POST", "/api/v1/auth/totp/disable",
		map[string]string{"password": ownerPass, "code": "000000"})
	c2.want(http.StatusNoContent, "POST", "/api/v1/auth/totp/disable",
		map[string]string{"password": ownerPass, "code": auth.TOTPCodeAt(secret, e.clock.Now())})
	if r := e.client().login(ownerEmail, ownerPass); r.body["mfa_required"] != false {
		t.Fatal("2FA still required after disabling")
	}
}

func TestMFAPendingSessionExpiresQuickly(t *testing.T) {
	e := newEnv(t)
	c := e.setupOwner()
	r := c.want(200, "POST", "/api/v1/auth/totp/setup", map[string]string{"password": ownerPass})
	secret, _ := auth.DecodeTOTPSecret(r.body["secret"].(string))
	c.want(200, "POST", "/api/v1/auth/totp/enable", map[string]string{"code": auth.TOTPCodeAt(secret, e.clock.Now())})

	c2 := e.client()
	c2.login(ownerEmail, ownerPass)
	e.clock.Advance(mfaPendingTimeout + time.Second)
	c2.want(http.StatusUnauthorized, "POST", "/api/v1/auth/mfa",
		map[string]string{"code": auth.TOTPCodeAt(secret, e.clock.Now())})
}

func TestEnablingTOTPRevokesOtherSessions(t *testing.T) {
	e := newEnv(t)
	c := e.setupOwner()
	other := e.client()
	other.login(ownerEmail, ownerPass)

	r := c.want(200, "POST", "/api/v1/auth/totp/setup", map[string]string{"password": ownerPass})
	secret, _ := auth.DecodeTOTPSecret(r.body["secret"].(string))
	c.want(200, "POST", "/api/v1/auth/totp/enable", map[string]string{"code": auth.TOTPCodeAt(secret, e.clock.Now())})
	other.want(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	e := newEnv(t)
	c := e.setupOwner()
	other := e.client()
	other.login(ownerEmail, ownerPass)

	newPass := "a completely new passphrase"
	c.want(http.StatusForbidden, "POST", "/api/v1/auth/password",
		map[string]string{"current_password": "wrong password!!", "new_password": newPass})
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/auth/password",
		map[string]string{"current_password": ownerPass, "new_password": "short"})
	c.want(http.StatusNoContent, "POST", "/api/v1/auth/password",
		map[string]string{"current_password": ownerPass, "new_password": newPass})

	c.want(200, "GET", "/api/v1/auth/me", nil)
	other.want(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
	if r := e.client().login(ownerEmail, newPass); r.status != 200 {
		t.Fatal("new password does not work")
	}
}

func TestRoles(t *testing.T) {
	e := newEnv(t)
	owner := e.setupOwner()
	pass := "viewer password 123"

	owner.want(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "admin@example.com", "name": "Admin", "password": pass, "role": "admin"})
	owner.want(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "viewer@example.com", "name": "Viewer", "password": pass, "role": "viewer"})
	owner.want(http.StatusConflict, "POST", "/api/v1/users", map[string]string{
		"email": "VIEWER@example.com", "name": "Dup", "password": pass, "role": "viewer"})
	owner.want(http.StatusUnprocessableEntity, "POST", "/api/v1/users", map[string]string{
		"email": "x@example.com", "name": "X", "password": pass, "role": "superuser"})

	admin := e.client()
	admin.login("admin@example.com", pass)
	admin.want(200, "GET", "/api/v1/users", nil)
	admin.want(http.StatusForbidden, "POST", "/api/v1/users", map[string]string{
		"email": "o2@example.com", "name": "O2", "password": pass, "role": "owner"})

	viewer := e.client()
	viewer.login("viewer@example.com", pass)
	viewer.want(200, "GET", "/api/v1/auth/me", nil)
	viewer.want(http.StatusForbidden, "GET", "/api/v1/users", nil)
	viewer.want(http.StatusForbidden, "POST", "/api/v1/users", map[string]string{
		"email": "v2@example.com", "name": "V2", "password": pass, "role": "viewer"})

	r := owner.want(200, "GET", "/api/v1/users", nil)
	if n := len(r.body["users"].([]any)); n != 3 {
		t.Fatalf("%d users; want 3", n)
	}
	for _, u := range r.body["users"].([]any) {
		if _, leaked := u.(map[string]any)["password_hash"]; leaked {
			t.Fatal("password hash exposed in API")
		}
	}
}

func TestLoginRateLimitPerAccount(t *testing.T) {
	e := newEnv(t)
	e.setupOwner()
	c := e.client()
	for i := range 5 {
		if r := c.login(ownerEmail, "wrong password!!"); r.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.status)
		}
	}
	r := c.login(ownerEmail, ownerPass)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("status %d; want 429 with Retry-After", r.status)
	}
}

func TestAuditLogRecordsLogins(t *testing.T) {
	e := newEnv(t)
	e.setupOwner()
	e.client().login(ownerEmail, "wrong password!!")
	entries, err := e.srv.db.RecentAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range entries {
		actions = append(actions, a.Action)
		if b, _ := json.Marshal(a.Details); strings.Contains(string(b), ownerPass) {
			t.Fatal("password leaked into audit log")
		}
	}
	if got := strings.Join(actions, ","); got != "login.failed,setup.completed" {
		t.Fatalf("audit actions = %s", got)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/auth/login",
		strings.NewReader(`{"email":"a@b.c","password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d; want 403", r.StatusCode)
	}
}

func TestJSONStrictness(t *testing.T) {
	e := newEnv(t)
	post := func(ct, body string) int {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if s := post("text/plain", `{"email":"a@b.c","password":"x"}`); s != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", s)
	}
	if s := post("application/json", `{"email":"a@b.c","password":"x","admin":true}`); s != http.StatusBadRequest {
		t.Errorf("unknown field: %d", s)
	}
	if s := post("application/json", `{"email":"a@b.c"}{}`); s != http.StatusBadRequest {
		t.Errorf("trailing data: %d", s)
	}
	if s := post("application/json", `{"email":"`+strings.Repeat("a", maxJSONBody)+`"}`); s != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", s)
	}
}

func TestSecureCookieAttributes(t *testing.T) {
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, SecureCookies: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/setup", strings.NewReader(
		`{"setup_token":"`+testSetupToken+`","email":"`+ownerEmail+`","name":"O","password":"`+ownerPass+`"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	c := rec.Result().Cookies()[0]
	if c.Name != "__Host-basalt_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("cookie = %+v", c)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
