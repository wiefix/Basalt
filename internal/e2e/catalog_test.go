package e2e

import (
	"net/http"
	"strings"
	"testing"
)

func TestCatalogAPI(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p) // no Docker: deployments fail on the agent
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	list := p.call(200, "GET", "/api/v1/catalog", nil)["templates"].([]any)
	var wp map[string]any
	for _, tpl := range list {
		if tpl := tpl.(map[string]any); tpl["id"] == "wordpress" {
			wp = tpl
		}
	}
	if wp == nil || !strings.Contains(wp["compose"].(string), "@sha256:") || len(wp["services"].([]any)) != 2 ||
		wp["inputs"].([]any)[0].(map[string]any)["type"] != "domain" {
		t.Fatalf("wordpress = %v", wp)
	}

	install := func(want int, id string, body map[string]any) map[string]any {
		return p.call(want, "POST", "/api/v1/catalog/"+id+"/install", body)
	}
	install(http.StatusNotFound, "nope", map[string]any{"server_id": sid})
	install(http.StatusUnprocessableEntity, "wordpress", map[string]any{"server_id": sid})
	install(http.StatusUnprocessableEntity, "wordpress", map[string]any{"server_id": sid, "inputs": map[string]string{"DOMAIN": "not a domain"}})
	install(http.StatusUnprocessableEntity, "wordpress", map[string]any{"server_id": sid, "inputs": map[string]string{"DOMAIN": "blog.localhost", "DB_PASSWORD": "mine"}})
	install(http.StatusUnprocessableEntity, "wordpress", map[string]any{"server_id": sid, "name": "bad name!", "inputs": map[string]string{"DOMAIN": "blog.localhost"}})
	install(http.StatusUnprocessableEntity, "wordpress", map[string]any{"server_id": 999, "inputs": map[string]string{"DOMAIN": "blog.localhost"}})
	r := install(http.StatusCreated, "wordpress", map[string]any{"server_id": sid, "inputs": map[string]string{"DOMAIN": "Blog.localhost"}})
	app := r["app"].(map[string]any)
	aid := jsonID(app["id"])
	if app["name"] != "wordpress" || app["source"] != "compose" || app["template"] != "wordpress" || app["template_revision"] != float64(1) ||
		app["service"] != "wordpress" || app["port"] != float64(80) || jsonString(app["domains"]) != `["blog.localhost"]` ||
		r["deployment"] == nil || !strings.Contains(r["notes"].(string), "administrator") {
		t.Fatalf("install = %v", r)
	}
	// The secrets are generated, different, stored encrypted only.
	env := p.call(200, "GET", "/api/v1/apps/"+aid+"/env", nil)["env"].(map[string]any)
	pw, root := env["DB_PASSWORD"].(string), env["DB_ROOT_PASSWORD"].(string)
	if len(pw) != 32 || len(root) != 32 || pw == root || env["DOMAIN"] != "blog.localhost" {
		t.Fatalf("env = %v", env)
	}
	var sealed []byte
	p.db.R.QueryRow(`SELECT env FROM apps WHERE id = ?`, mustAtoi(aid)).Scan(&sealed)
	if strings.Contains(string(sealed), pw) {
		t.Fatal("secret stored in clear")
	}
	// The same domain or name again conflicts; a second copy needs both new.
	install(http.StatusConflict, "wordpress", map[string]any{"server_id": sid, "name": "wp2", "inputs": map[string]string{"DOMAIN": "blog.localhost"}})
	install(http.StatusConflict, "wordpress", map[string]any{"server_id": sid, "inputs": map[string]string{"DOMAIN": "blog2.localhost"}})
	second := install(http.StatusCreated, "wordpress", map[string]any{"server_id": sid, "name": "wp2",
		"inputs": map[string]string{"DOMAIN": "blog2.localhost"}})["app"].(map[string]any)
	env2 := p.call(200, "GET", "/api/v1/apps/"+jsonID(second["id"])+"/env", nil)["env"].(map[string]any)
	if env2["DB_PASSWORD"] == pw {
		t.Fatal("secrets shared between installations")
	}
	// The installed app is an ordinary compose app: its file is the template's.
	cf := p.call(200, "GET", "/api/v1/apps/"+aid+"/compose", nil)
	if !strings.Contains(cf["compose"].(string), "${DB_PASSWORD}") {
		t.Fatalf("compose = %v", cf)
	}

	// Viewers browse the catalog but do not install.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(200, "GET", "/api/v1/catalog", nil)
	viewer.call(http.StatusForbidden, "POST", "/api/v1/catalog/wordpress/install", map[string]any{"server_id": sid,
		"inputs": map[string]string{"DOMAIN": "blog3.localhost"}})
	// The audit log names the template, not the secrets.
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(details) FROM audit_log WHERE action = 'app.installed'`).Scan(&audit)
	if !strings.Contains(audit, `"template":"wordpress"`) || strings.Contains(audit, pw) {
		t.Fatalf("audit: %s", audit)
	}
}
