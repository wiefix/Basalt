package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAPITokens: tokens act as their user, capped by their role, cannot
// manage credentials, and stop working when revoked, expired or their user
// is disabled.
func TestAPITokens(t *testing.T) {
	p := startPanel(t)
	bearer := func(token string, want int, method, path string, body any) map[string]any {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, p.api.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req) // no cookies
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("%s %s with token: %d %v; want %d", method, path, resp.StatusCode, out, want)
		}
		return out
	}
	create := func(c *panel, want int, body map[string]any) map[string]any {
		t.Helper()
		return c.call(want, "POST", "/api/v1/account/tokens", body)
	}
	const pw = "owner password 123"

	for _, bad := range []map[string]any{
		{"name": "", "role": "member", "password": pw},
		{"name": " ci", "role": "member", "password": pw},
		{"name": "ci\n", "role": "member", "password": pw},
		{"name": strings.Repeat("x", 65), "role": "member", "password": pw},
		{"name": "ci", "role": "root", "password": pw},
		{"name": "ci", "role": "member", "expires_days": -1, "password": pw},
		{"name": "ci", "role": "member", "expires_days": 5000, "password": pw},
	} {
		create(p, http.StatusUnprocessableEntity, bad)
	}
	create(p, http.StatusForbidden, map[string]any{"name": "ci", "role": "member", "password": "wrong"})

	member := create(p, http.StatusCreated, map[string]any{"name": "ci", "role": "member", "expires_days": 30, "password": pw})
	secret := member["secret"].(string)
	tok := member["token"].(map[string]any)
	if !strings.HasPrefix(secret, "bst_") || !strings.HasPrefix(secret, tok["prefix"].(string)) || tok["expires_at"] == nil {
		t.Fatalf("created: %v", member)
	}
	var stored int
	p.db.R.QueryRow(`SELECT count(*) FROM api_tokens WHERE instr(CAST(token_hash AS TEXT), ?) > 0`, secret).Scan(&stored)
	if stored != 0 {
		t.Fatal("token stored in the clear")
	}
	create(p, http.StatusConflict, map[string]any{"name": "ci", "role": "viewer", "password": pw})

	// The token acts as the owner with the member role.
	if me := bearer(secret, 200, "GET", "/api/v1/auth/me", nil); me["user"].(map[string]any)["role"] != "member" {
		t.Fatalf("me: %v", me)
	}
	bearer(secret, 200, "GET", "/api/v1/servers", nil)
	bearer(secret, http.StatusForbidden, "POST", "/api/v1/servers", map[string]string{"name": "node"})
	// Credentials are managed in the panel only.
	bearer(secret, http.StatusForbidden, "GET", "/api/v1/account/tokens", nil)
	bearer(secret, http.StatusForbidden, "POST", "/api/v1/account/tokens", map[string]any{"name": "x", "role": "viewer", "password": pw})
	bearer(secret, http.StatusForbidden, "POST", "/api/v1/auth/password", map[string]string{"current_password": pw, "new_password": "x"})
	bearer(secret, http.StatusUnauthorized, "POST", "/api/v1/auth/logout", map[string]string{})
	bearer("bst_nope", http.StatusUnauthorized, "GET", "/api/v1/servers", nil)
	bearer("garbage", http.StatusUnauthorized, "GET", "/api/v1/servers", nil)

	// An admin token acts as admin; the audit log names the token.
	admin := create(p, http.StatusCreated, map[string]any{"name": "deploy bot", "role": "admin", "password": pw})["secret"].(string)
	bearer(admin, http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "node"})
	var details string
	p.db.R.QueryRow(`SELECT details FROM audit_log WHERE action = 'server.created'`).Scan(&details)
	if !strings.Contains(details, `"api_token":"deploy bot"`) {
		t.Fatalf("audit: %s", details)
	}
	mine := p.call(200, "GET", "/api/v1/account/tokens", nil)["tokens"].([]any)
	if len(mine) != 2 || mine[0].(map[string]any)["last_used_at"] == nil || mine[0].(map[string]any)["last_used_ip"] == "" {
		t.Fatalf("own tokens: %v", mine)
	}

	// A viewer's token never exceeds viewer.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	create(&viewer, http.StatusUnprocessableEntity, map[string]any{"name": "x", "role": "admin", "password": "viewer password 1"})
	vt := create(&viewer, http.StatusCreated, map[string]any{"name": "read", "role": "viewer", "password": "viewer password 1"})
	vsecret := vt["secret"].(string)
	vid := vt["token"].(map[string]any)["id"]
	bearer(vsecret, 200, "GET", "/api/v1/servers", nil)
	viewer.call(http.StatusForbidden, "GET", "/api/v1/tokens", nil)
	// Others' tokens are not reachable through one's own list.
	ownerTokenID := mine[0].(map[string]any)["id"]
	viewer.call(http.StatusNotFound, "DELETE", fmt.Sprintf("/api/v1/account/tokens/%v", ownerTokenID), nil)

	// An admin sees every token and revokes them.
	all := p.call(200, "GET", "/api/v1/tokens", nil)["tokens"].([]any)
	if len(all) != 3 || all[0].(map[string]any)["user"].(map[string]any)["email"] != "v@example.com" {
		t.Fatalf("all tokens: %v", all)
	}
	p.call(http.StatusNoContent, "DELETE", fmt.Sprintf("/api/v1/tokens/%v", vid), nil)
	bearer(vsecret, http.StatusUnauthorized, "GET", "/api/v1/servers", nil)

	// Expired tokens and tokens of disabled users stop working.
	p.db.W.Exec(`UPDATE api_tokens SET expires_at = ? WHERE name = 'ci'`, time.Now().Add(-time.Minute).Unix())
	bearer(secret, http.StatusUnauthorized, "GET", "/api/v1/servers", nil)
	if l := p.call(200, "GET", "/api/v1/account/tokens", nil)["tokens"].([]any); l[1].(map[string]any)["expired"] != true {
		t.Fatalf("expired flag: %v", l)
	}
	bearer(admin, 200, "GET", "/api/v1/servers", nil)
	p.db.W.Exec(`UPDATE users SET disabled_at = unixepoch() WHERE email = 'o@example.com'`)
	bearer(admin, http.StatusUnauthorized, "GET", "/api/v1/servers", nil)
	p.db.W.Exec(`UPDATE users SET disabled_at = NULL WHERE email = 'o@example.com'`)

	// Deleting one's own token.
	p.call(http.StatusNoContent, "DELETE", fmt.Sprintf("/api/v1/account/tokens/%v", ownerTokenID), nil)
	bearer(admin, http.StatusUnauthorized, "GET", "/api/v1/servers", nil)
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(action) FROM (SELECT action FROM audit_log WHERE action LIKE 'token.%' ORDER BY id)`).Scan(&audit)
	if audit != "token.created,token.created,token.created,token.revoked,token.revoked" {
		t.Fatalf("audit: %s", audit)
	}
}
