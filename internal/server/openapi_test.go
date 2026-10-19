package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestOpenAPICoversRoutes: every API route is described, with the access
// its registration requires, and no description is left without a route.
func TestOpenAPICoversRoutes(t *testing.T) {
	e := newEnv(t)
	e.client().want(http.StatusUnauthorized, "GET", "/api/v1/openapi.json", nil)
	spec := e.setupOwner().want(http.StatusOK, "GET", "/api/v1/openapi.json", nil).body
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("version: %v", spec["openapi"])
	}
	paths := spec["paths"].(map[string]any)
	op := func(method, path string) map[string]any {
		t.Helper()
		o, ok := paths[path].(map[string]any)[strings.ToLower(method)].(map[string]any)
		if !ok {
			t.Fatalf("%s %s not described", method, path)
		}
		return o
	}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for path, methods := range paths {
		for method, o := range methods.(map[string]any) {
			o := o.(map[string]any)
			key := strings.ToUpper(method) + " " + path
			if o["summary"] == "" {
				t.Errorf("%s has no description in apiDocs", key)
			}
			// A JSON route describes its response.
			if r := o["responses"].(map[string]any); r["204"] == nil && r["2XX"].(map[string]any)["content"] == nil {
				t.Errorf("%s has no response in apiDocs", key)
			}
			if id := o["operationId"].(string); ids[id] {
				t.Errorf("duplicate operationId %s", id)
			} else {
				ids[id] = true
			}
			seen[key] = true
			for _, target := range backupTargets {
				if rest, ok := strings.CutPrefix(path, target); ok {
					seen[strings.ToUpper(method)+" {target}"+rest] = true
				}
			}
		}
	}
	for key := range apiDocs {
		if !seen[key] {
			t.Errorf("apiDocs describes %s, which is not a route", key)
		}
	}

	// Access comes from the registrations.
	if o := op("POST", "/api/v1/auth/login"); len(o["security"].([]any)) != 0 {
		t.Errorf("login: %v", o["security"])
	}
	if o := op("GET", "/api/v1/users"); o["x-basalt-role"] != "admin" || o["security"] != nil {
		t.Errorf("list users: %v", o)
	}
	if o := op("POST", "/api/v1/account/tokens"); o["x-basalt-role"] != nil || !strings.Contains(o["description"].(string), "not an API token") {
		t.Errorf("create token: %v", o)
	}
	if o := op("GET", "/api/v1/apps/{id}/backups"); o["x-basalt-role"] != "viewer" || o["x-basalt-project"] != true || o["tags"].([]any)[0] != "backups" {
		t.Errorf("app backups: %v", o)
	}
	// Bodies are described from their Go types.
	body := op("POST", "/api/v1/sites")["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if body["$ref"] != "#/components/schemas/CreateSiteRequest" {
		t.Fatalf("site body: %v", body)
	}
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	site := schemas["CreateSiteRequest"].(map[string]any)["properties"].(map[string]any)
	for _, f := range []string{"server_id", "name", "domains", "tls", "php_version"} {
		if site[f] == nil {
			t.Errorf("CreateSiteRequest lacks %s: %v", f, site)
		}
	}
	if rec := schemas["DnsRecord"].(map[string]any)["properties"].(map[string]any); rec["health_check"] == nil || rec["ttl"] == nil {
		t.Errorf("DnsRecord: %v", rec)
	}
	relay := schemas["MailSettingsRequest"].(map[string]any)["properties"].(map[string]any)["relay"].(map[string]any)["oneOf"].([]any)
	if relay[0].(map[string]any)["$ref"] != "#/components/schemas/MailRelayRequest" {
		t.Errorf("relay: %v", relay)
	}
	if _, ok := op("PUT", "/api/v1/sites/{id}/files/content")["requestBody"].(map[string]any)["content"].(map[string]any)["application/octet-stream"]; !ok {
		t.Error("upload is not a binary body")
	}
	if params := op("GET", "/api/v1/apps/{id}/logs")["parameters"].([]any); len(params) != 4 {
		t.Errorf("logs parameters: %v", params)
	}
}
