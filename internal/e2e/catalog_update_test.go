package e2e

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/server"
)

const whoamiImage = "traefik/whoami:v1.10@sha256:1699d99cb4b9acc17f74ca670b3d8d0b7ba27c948b3445f0593b58ebece92f04"

// demoTemplate is a whoami app; revision 2 greets differently and asks
// for one more input and one more secret.
func demoTemplate(t *testing.T, revision int) *catalog.Template {
	t.Helper()
	meta := `
id: demo
name: Demo
version: "` + map[int]string{1: "1.0", 2: "2.0"}[revision] + `"
revision: ` + map[int]string{1: "1", 2: "2"}[revision] + `
inputs:
  - {key: DOMAIN, label: Domain, type: domain, required: true}
secrets:
  - {key: TOKEN, length: 24}
expose: {service: web, port: 80}
notes: hello
`
	compose := `
services:
  web:
    image: ` + whoamiImage + `
    environment:
      WHOAMI_NAME: one
      TOKEN: ${TOKEN}
    volumes: [data:/data]
volumes:
  data:
`
	if revision == 2 {
		meta = strings.Replace(meta, "secrets:", "  - {key: OWNER, label: Owner, type: text, required: true}\nsecrets:\n  - {key: NEW_KEY, length: 32, charset: hex}", 1)
		compose = strings.Replace(compose, "WHOAMI_NAME: one", "WHOAMI_NAME: two-${OWNER}\n      NEW_KEY: ${NEW_KEY}", 1)
	}
	tpl, err := catalog.Parse(context.Background(), []byte(meta), []byte(compose))
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}

// switchableCatalog serves revision 1 of the demo template until set to 2.
type switchableCatalog struct {
	mu   sync.Mutex
	list []*catalog.Template
}

func (c *switchableCatalog) set(tpls ...*catalog.Template) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = tpls
}

func (c *switchableCatalog) get() ([]*catalog.Template, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list, nil
}

func TestTemplateUpdateAPI(t *testing.T) {
	cat := &switchableCatalog{}
	cat.set(demoTemplate(t, 1))
	p := startPanelWith(t, server.Options{Catalog: cat.get})
	h := startHost(t, p) // no Docker: deployments and volume backups fail on the agent
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	app := p.call(http.StatusCreated, "POST", "/api/v1/catalog/demo/install", map[string]any{"server_id": sid,
		"inputs": map[string]string{"DOMAIN": "demo.localhost"}})["app"].(map[string]any)
	base := "/api/v1/apps/" + jsonID(app["id"])
	env1 := p.call(200, "GET", base+"/env", nil)["env"].(map[string]any)
	if tpl := p.call(200, "GET", base+"/template", nil); tpl["update_available"] != false || tpl["installed_revision"] != float64(1) {
		t.Fatalf("template = %v", tpl)
	}
	p.call(http.StatusConflict, "POST", base+"/template-update", map[string]any{})

	// Revision 2 appears in the catalog.
	cat.set(demoTemplate(t, 2))
	tpl := p.call(200, "GET", base+"/template", nil)
	if tpl["update_available"] != true || jsonString(tpl["images"]) != "[]" || !strings.Contains(jsonString(tpl["needs"]), `"key":"OWNER"`) ||
		tpl["backups"] != false {
		t.Fatalf("template = %v", tpl)
	}
	apps := p.call(200, "GET", "/api/v1/apps", nil)["apps"].([]any)
	if apps[0].(map[string]any)["template_update"] != "2.0" {
		t.Fatalf("apps = %v", apps)
	}
	p.call(http.StatusUnprocessableEntity, "POST", base+"/template-update", map[string]any{})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"OWNER": "x", "NOPE": "y"}})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"OWNER": "a\nb"}})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"DOMAIN": "x.localhost", "OWNER": "x"}})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"OWNER": "x"}, "backup": true})

	// With a backup that fails, nothing changes.
	// The panel only needs to believe restic is there (as with Docker
	// above): the backup fails on the agent all the same.
	if _, err := p.db.W.Exec(`UPDATE servers SET restic = '0.19.1' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": filepath.Join(t.TempDir(), "backups")})["storage"].(map[string]any)
	p.call(200, "PUT", base+"/backup-policy", map[string]any{"storage_id": st["id"], "schedule": "manual", "keep_last": 3})
	u := p.call(http.StatusAccepted, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"OWNER": "boss"}, "backup": true})["update"].(map[string]any)
	if u["state"] != "backing_up" || u["backup_id"] == nil {
		t.Fatalf("update = %v", u)
	}
	var failed map[string]any
	eventually(t, "update failed with the backup", func() bool {
		failed, _ = p.call(200, "GET", base+"/template", nil)["update"].(map[string]any)
		return failed != nil && failed["state"] == "failed"
	})
	if !strings.Contains(failed["error"].(string), "backup failed") || p.call(200, "GET", base, nil)["app"].(map[string]any)["template_revision"] != float64(1) {
		t.Fatalf("failed update = %v", failed)
	}

	// Without a backup it applies: values kept, the new secret generated.
	p.call(http.StatusAccepted, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"OWNER": "boss"}})
	eventually(t, "template applied", func() bool {
		return p.call(200, "GET", base, nil)["app"].(map[string]any)["template_revision"] == float64(2)
	})
	env2 := p.call(200, "GET", base+"/env", nil)["env"].(map[string]any)
	if env2["TOKEN"] != env1["TOKEN"] || env2["OWNER"] != "boss" || len(env2["NEW_KEY"].(string)) != 32 || env2["DOMAIN"] != "demo.localhost" {
		t.Fatalf("env = %v (was %v)", env2, env1)
	}
	if cf := p.call(200, "GET", base+"/compose", nil)["compose"].(string); !strings.Contains(cf, "two-${OWNER}") {
		t.Fatalf("compose = %s", cf)
	}
	if tpl := p.call(200, "GET", base+"/template", nil); tpl["update_available"] != false || tpl["update"] != nil {
		t.Fatalf("template = %v", tpl)
	}
	deps := p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any)
	if len(deps) < 2 {
		t.Fatalf("deployments = %v", deps)
	}
	p.call(http.StatusConflict, "POST", base+"/template-update", map[string]any{})

	// Viewers see whether an update exists but do not update.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(200, "GET", base+"/template", nil)
	viewer.call(http.StatusForbidden, "POST", base+"/template-update", map[string]any{})
	// An app not from the catalog has no template.
	plain := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "plain",
		"image": "traefik/whoami:v1.10", "port": 80})["app"].(map[string]any)
	p.call(http.StatusNotFound, "GET", "/api/v1/apps/"+jsonID(plain["id"])+"/template", nil)
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(details) FROM (SELECT details FROM audit_log WHERE action = 'app.template_update' ORDER BY id)`).Scan(&audit)
	if !strings.Contains(audit, `"to":2`) || strings.Contains(audit, env2["NEW_KEY"].(string)) {
		t.Fatalf("audit: %s", audit)
	}
}

func TestTemplateUpdateWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	cat := &switchableCatalog{}
	cat.set(demoTemplate(t, 1))
	p := startPanelWith(t, server.Options{Catalog: cat.get})
	h := startHostWith(t, p, sock)
	sid := mustAtoi(h.id)
	r := p.call(http.StatusCreated, "POST", "/api/v1/catalog/demo/install", map[string]any{"server_id": sid,
		"inputs": map[string]string{"DOMAIN": "upd.localhost"}})
	base := "/api/v1/apps/" + jsonID(r["app"].(map[string]any)["id"])
	t.Cleanup(func() { p.call(200, "DELETE", base+"?volumes=1", nil) })
	d1, log := followDeployment(t, p, base, jsonID(r["deployment"].(map[string]any)["id"]))
	if d1["status"] != "succeeded" {
		t.Fatalf("install: %v\n%s", d1, log)
	}
	served := func(name string) func() bool {
		return func() bool { _, body := h.get("upd.localhost", "/"); return strings.Contains(body, "Name: "+name) }
	}
	eventually(t, "revision 1 served", served("one"))

	eventually(t, "restic reported", func() bool { return p.server(h.id)["restic"] != "" })
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": filepath.Join(t.TempDir(), "backups")})["storage"].(map[string]any)
	p.call(200, "PUT", base+"/backup-policy", map[string]any{"storage_id": st["id"], "schedule": "manual", "keep_last": 3})

	cat.set(demoTemplate(t, 2))
	u := p.call(http.StatusAccepted, "POST", base+"/template-update", map[string]any{"inputs": map[string]string{"OWNER": "boss"}, "backup": true})["update"].(map[string]any)
	bid := jsonID(u["backup_id"])
	deadline := time.Now().Add(3 * time.Minute)
	for !served("two-boss")() {
		if time.Now().After(deadline) {
			t.Fatalf("update not served: %v; backup %v", p.call(200, "GET", base+"/template", nil),
				p.call(200, "GET", base+"/backups/"+bid, nil))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if b := p.call(200, "GET", base+"/backups/"+bid, nil)["backup"].(map[string]any); b["status"] != "succeeded" {
		t.Fatalf("backup = %v", b)
	}
	eventually(t, "update deployment finished", func() bool {
		deps := p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any)
		return deps[0].(map[string]any)["status"] == "succeeded"
	})
	// Rolling back the deployment goes back to revision 1's file.
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{"rollback_to": d1["id"]})["deployment"].(map[string]any)
	if d, log := followDeployment(t, p, base, jsonID(dep["id"])); d["status"] != "succeeded" {
		t.Fatalf("rollback: %v\n%s", d, log)
	}
	eventually(t, "rolled back", served("one"))
}
