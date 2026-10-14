package e2e

import (
	"net/http"
	"strings"
	"testing"
)

func TestRegistries(t *testing.T) {
	p := startPanel(t)
	const secret = "ghp_s3cr3tT0k3n"

	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/registries", map[string]string{"host": "not a host", "username": "u", "password": "p"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/registries", map[string]string{"host": "ghcr.io", "username": "u"})
	r := p.call(http.StatusCreated, "POST", "/api/v1/registries", map[string]string{"host": "https://GHCR.io/", "username": "bot", "password": secret})
	reg := r["registry"].(map[string]any)
	if reg["host"] != "ghcr.io" || reg["username"] != "bot" {
		t.Fatalf("registry = %v", reg)
	}
	id := jsonID(reg["id"])
	p.call(http.StatusConflict, "POST", "/api/v1/registries", map[string]string{"host": "ghcr.io", "username": "x", "password": "y"})

	// The secret is never returned, stored only encrypted, never audited.
	list := p.call(200, "GET", "/api/v1/registries", nil)
	if s := jsonString(list); strings.Contains(s, secret) || !strings.Contains(s, "ghcr.io") {
		t.Fatalf("list = %s", s)
	}
	var stored []byte
	p.db.R.QueryRow(`SELECT secret FROM registries WHERE id = ?`, mustAtoi(id)).Scan(&stored)
	if len(stored) == 0 || strings.Contains(string(stored), secret) {
		t.Fatal("registry secret stored in clear")
	}
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(details) FROM (SELECT details FROM audit_log WHERE action LIKE 'registry.%' ORDER BY id)`).Scan(&audit)
	if strings.Contains(audit, secret) {
		t.Fatalf("secret in audit log: %s", audit)
	}

	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/registries/"+id, map[string]string{"host": "quay.io", "username": "bot", "password": "z"})
	r = p.call(200, "PUT", "/api/v1/registries/"+id, map[string]string{"username": "bot2", "password": "new-token"})
	if r["registry"].(map[string]any)["username"] != "bot2" {
		t.Fatalf("update = %v", r)
	}

	for _, role := range []string{"member", "viewer"} {
		email := role + "@example.com"
		p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
			"email": email, "name": role, "password": role + " password 123", "role": role,
		})
		u := *p
		u.hc = &http.Client{Jar: newJar()}
		u.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": email, "password": role + " password 123"})
		u.call(http.StatusForbidden, "GET", "/api/v1/registries", nil)
		u.call(http.StatusForbidden, "DELETE", "/api/v1/registries/"+id, nil)
	}

	p.call(http.StatusNoContent, "DELETE", "/api/v1/registries/"+id, nil)
	p.call(http.StatusNotFound, "DELETE", "/api/v1/registries/"+id, nil)
}
