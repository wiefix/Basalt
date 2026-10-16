package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/webauthn/webauthntest"
)

var b64 = base64.RawURLEncoding

// TestPasskeys adds passkeys and signs in with them, through the API as a
// browser would (Origin header, the panel's host name), with a software
// authenticator.
func TestPasskeys(t *testing.T) {
	p := startPanel(t)
	port := strings.TrimPrefix(p.api.URL, "http://127.0.0.1")
	host := "localhost" + port
	origin := "http://" + host

	// send calls the API as the browser on origin, with the Host header host.
	send := func(c *panel, want int, method, path string, body any, origin, host string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, c.api.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		// The jar keys cookies by URL; with another Host header they are
		// handed over by hand.
		req.Host = host
		u := mustURL(c.api.URL)
		for _, ck := range c.hc.Jar.Cookies(u) {
			req.AddCookie(ck)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		c.hc.Jar.SetCookies(u, resp.Cookies())
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, raw)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		return m
	}
	browser := func(c *panel, want int, method, path string, body any) map[string]any {
		t.Helper()
		return send(c, want, method, path, body, origin, host)
	}
	options := func(c *panel, password string) (challenge, handle []byte) {
		t.Helper()
		o := browser(c, 200, "POST", "/api/v1/account/passkeys/options", map[string]any{"password": password})["options"].(map[string]any)
		if o["rp"].(map[string]any)["id"] != "localhost" || o["attestation"] != "none" ||
			o["authenticatorSelection"].(map[string]any)["userVerification"] != "required" {
			t.Fatalf("options %v", o)
		}
		challenge, _ = b64.DecodeString(o["challenge"].(string))
		handle, _ = b64.DecodeString(o["user"].(map[string]any)["id"].(string))
		return challenge, handle
	}
	add := func(c *panel, want int, a *webauthntest.Authenticator, name, password string) map[string]any {
		t.Helper()
		ch, handle := options(c, password)
		reg := a.Create("localhost", origin, ch, handle)
		return browser(c, want, "POST", "/api/v1/account/passkeys", map[string]any{"name": name,
			"client_data_json": b64.EncodeToString(reg.ClientDataJSON), "attestation_object": b64.EncodeToString(reg.AttestationObject)})
	}
	anon := func() *panel {
		c := *p
		c.hc = &http.Client{Jar: newJar()}
		return &c
	}
	login := func(c *panel, want int, a *webauthntest.Authenticator) map[string]any {
		t.Helper()
		o := browser(c, 200, "POST", "/api/v1/auth/passkey/options", map[string]any{})
		ch, _ := b64.DecodeString(o["options"].(map[string]any)["challenge"].(string))
		as := a.Get("localhost", origin, ch)
		return browser(c, want, "POST", "/api/v1/auth/passkey", map[string]any{"challenge_id": o["challenge_id"],
			"credential_id": b64.EncodeToString(as.CredentialID), "client_data_json": b64.EncodeToString(as.ClientDataJSON),
			"authenticator_data": b64.EncodeToString(as.AuthenticatorData), "signature": b64.EncodeToString(as.Signature),
			"user_handle": b64.EncodeToString(as.UserHandle)})
	}

	// Where passkeys cannot be made: no Origin, an IP address, plain http
	// on a public name.
	send(p, http.StatusUnprocessableEntity, "POST", "/api/v1/account/passkeys/options", map[string]any{"password": "owner password 123"}, "", host)
	send(p, http.StatusUnprocessableEntity, "POST", "/api/v1/account/passkeys/options", map[string]any{"password": "owner password 123"}, "http://127.0.0.1"+port, "127.0.0.1"+port)
	// (another site's origin is stopped before: cross-origin protection)
	send(p, http.StatusForbidden, "POST", "/api/v1/account/passkeys/options", map[string]any{"password": "owner password 123"}, "http://evil.example", host)
	send(p, http.StatusUnprocessableEntity, "POST", "/api/v1/account/passkeys/options", map[string]any{"password": "owner password 123"}, "http://panel.example.com", "panel.example.com")
	browser(p, http.StatusForbidden, "POST", "/api/v1/account/passkeys/options", map[string]any{"password": "wrong"})

	laptop := webauthntest.New(-7)
	k := add(p, http.StatusCreated, laptop, "laptop", "owner password 123")["passkey"].(map[string]any)
	if k["name"] != "laptop" || k["host"] != "localhost" || k["synced"] != false {
		t.Fatalf("passkey %v", k)
	}
	// The challenge is used once; the same passkey or name again is refused.
	browser(p, http.StatusUnprocessableEntity, "POST", "/api/v1/account/passkeys", map[string]any{"name": "again",
		"client_data_json": "e30", "attestation_object": "oA"})
	add(p, http.StatusConflict, laptop, "laptop 2", "owner password 123")
	// Names are checked before the challenge is used. (Passkeys of another
	// site or without user verification: see the webauthn tests. Each
	// options call checks the password, which is rate limited.)
	ch, handle := options(p, "owner password 123")
	reg := webauthntest.New(-7).Create("localhost", origin, ch, handle)
	for _, bad := range []string{"", " padded", strings.Repeat("x", 65), "tab\there", "laptop"} {
		want := http.StatusUnprocessableEntity
		if bad == "laptop" {
			want = http.StatusConflict
		}
		browser(p, want, "POST", "/api/v1/account/passkeys", map[string]any{"name": bad,
			"client_data_json": b64.EncodeToString(reg.ClientDataJSON), "attestation_object": b64.EncodeToString(reg.AttestationObject)})
	}

	// Signing in: no password, no second factor.
	c := anon()
	if u := login(c, 200, laptop)["user"].(map[string]any); u["email"] != "o@example.com" {
		t.Fatalf("signed in as %v", u)
	}
	c.call(200, "GET", "/api/v1/auth/me", nil)
	if list := p.call(200, "GET", "/api/v1/account/passkeys", nil)["passkeys"].([]any); len(list) != 1 || list[0].(map[string]any)["last_used_at"] == nil {
		t.Fatalf("passkeys %v", list)
	}
	// An answer is used once.
	o := browser(anon(), 200, "POST", "/api/v1/auth/passkey/options", map[string]any{})
	ch, _ = b64.DecodeString(o["options"].(map[string]any)["challenge"].(string))
	as := laptop.Get("localhost", origin, ch)
	body := map[string]any{"challenge_id": o["challenge_id"], "credential_id": b64.EncodeToString(as.CredentialID),
		"client_data_json": b64.EncodeToString(as.ClientDataJSON), "authenticator_data": b64.EncodeToString(as.AuthenticatorData),
		"signature": b64.EncodeToString(as.Signature), "user_handle": b64.EncodeToString(as.UserHandle)}
	browser(anon(), 200, "POST", "/api/v1/auth/passkey", body)
	browser(anon(), http.StatusUnauthorized, "POST", "/api/v1/auth/passkey", body)
	// Started on another host name of the panel.
	o = send(anon(), 200, "POST", "/api/v1/auth/passkey/options", map[string]any{}, "http://other.localhost"+port, "other.localhost"+port)
	body["challenge_id"] = o["challenge_id"]
	browser(anon(), http.StatusUnauthorized, "POST", "/api/v1/auth/passkey", body)

	// Unknown passkeys, a phishing origin, another user's handle.
	login(anon(), http.StatusUnauthorized, webauthntest.New(-7))
	phish := *laptop
	phish.Origin = "http://evil.example"
	login(anon(), http.StatusUnauthorized, &phish)
	stolen := *laptop
	stolen.UserHandle = []byte("someone else")
	login(anon(), http.StatusUnauthorized, &stolen)

	// A security key that counts: a counter going back means a clone.
	key := webauthntest.New(-257)
	key.Counter = 5
	add(p, http.StatusCreated, key, "yubikey", "owner password 123")
	login(anon(), 200, key)
	key.Counter = 2
	login(anon(), http.StatusUnauthorized, key)

	// Managing: rename, remove.
	list := p.call(200, "GET", "/api/v1/account/passkeys", nil)["passkeys"].([]any)
	keyID := jsonID(list[1].(map[string]any)["id"])
	p.call(http.StatusNoContent, "PUT", "/api/v1/account/passkeys/"+keyID, map[string]any{"name": "blue key"})
	p.call(http.StatusConflict, "PUT", "/api/v1/account/passkeys/"+keyID, map[string]any{"name": "laptop"})
	p.call(http.StatusNoContent, "DELETE", "/api/v1/account/passkeys/"+keyID, nil)
	p.call(http.StatusNotFound, "DELETE", "/api/v1/account/passkeys/"+keyID, nil)

	// Another user's passkeys: not theirs to remove; API tokens cannot manage them; disabled users cannot
	// sign in; an admin's "lost phone" reset removes them.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "member password 1", "role": "member"})
	m := anon()
	m.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "member password 1"})
	phone := webauthntest.New(-7)
	phone.Flags = 0x08 | 0x10
	if mk := add(m, http.StatusCreated, phone, "phone", "member password 1")["passkey"].(map[string]any); mk["synced"] != true {
		t.Fatalf("synced passkey %v", mk)
	}
	m.call(http.StatusNotFound, "DELETE", "/api/v1/account/passkeys/"+jsonID(k["id"]), nil)
	tok := m.call(http.StatusCreated, "POST", "/api/v1/account/tokens", map[string]any{"name": "ci", "role": "member", "password": "member password 1"})["secret"].(string)
	req, _ := http.NewRequest("GET", p.api.URL+"/api/v1/account/passkeys", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token: %v %v", resp, err)
	}
	var mid string
	for _, u := range p.call(200, "GET", "/api/v1/users", nil)["users"].([]any) {
		if u := u.(map[string]any); u["email"] == "m@example.com" {
			mid = jsonID(u["id"])
			if u["passkeys"] != float64(1) {
				t.Fatalf("users list %v", u)
			}
		}
	}
	p.call(200, "PUT", "/api/v1/users/"+mid, map[string]any{"disabled": true})
	login(anon(), http.StatusUnauthorized, phone)
	p.call(200, "PUT", "/api/v1/users/"+mid, map[string]any{"disabled": false})
	login(anon(), 200, phone)
	p.call(http.StatusNoContent, "PUT", "/api/v1/users/"+mid+"/password", map[string]any{"password": "new member password", "reset_totp": true})
	login(anon(), http.StatusUnauthorized, phone)

	var n int
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'login.succeeded' AND details LIKE '%passkey%'`).Scan(&n)
	var failed int
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'login.failed' AND details LIKE '%cloned%'`).Scan(&failed)
	if n != 4 || failed != 1 {
		t.Fatalf("audit: %d passkey sign-ins, %d clone warnings", n, failed)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
