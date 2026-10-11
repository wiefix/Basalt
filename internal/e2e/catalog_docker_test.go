package e2e

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

// TestCatalogWithDocker installs every catalog template and checks that
// the app works behind the proxy (with its database where it has one).
func TestCatalogWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" || os.Getenv("BASALT_CATALOG_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 BASALT_CATALOG_TESTS=1 to install every template (large images)")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	p := startPanel(t)
	h := startHostWith(t, p, sock)
	dc := docker.New(sock)
	// What shows each app works: a path and text in its answer.
	checks := map[string][2]string{
		"uptime-kuma": {"/api/entry-page", "type"},
		"vaultwarden": {"/alive", "20"},
		"n8n":         {"/healthz", "ok"},
		"wordpress":   {"/wp-admin/install.php", "WordPress"},
		"ghost":       {"/ghost/api/admin/site/", "site"},
		"gitea":       {"/api/healthz", "pass"},
	}
	list := p.call(200, "GET", "/api/v1/catalog", nil)["templates"].([]any)
	if len(list) != len(checks) {
		t.Fatalf("%d templates, %d checks", len(list), len(checks))
	}
	for _, tpl := range list {
		id := tpl.(map[string]any)["id"].(string)
		t.Run(id, func(t *testing.T) {
			check, ok := checks[id]
			if !ok {
				t.Fatalf("no check for %s", id)
			}
			domain := id + ".localhost"
			r := p.call(http.StatusCreated, "POST", "/api/v1/catalog/"+id+"/install", map[string]any{
				"server_id": mustAtoi(h.id), "inputs": map[string]string{"DOMAIN": domain}})
			aid := jsonID(r["app"].(map[string]any)["id"])
			base := "/api/v1/apps/" + aid
			t.Cleanup(func() {
				ctx := context.Background()
				list, _ := dc.ContainerList(ctx, map[string]string{"basalt.app": aid})
				for _, c := range list {
					dc.ContainerRemove(ctx, c.ID, true)
				}
				dc.NetworkRemove(ctx, "basalt-app-"+aid)
				vols, _ := dc.VolumeList(ctx, map[string]string{"basalt.app": aid})
				for _, v := range vols {
					dc.VolumeRemove(ctx, v.Name)
				}
			})
			d, log := followDeployment(t, p, base, jsonID(r["deployment"].(map[string]any)["id"]))
			if d["status"] != "succeeded" {
				t.Fatalf("deployment: %v\n%s", d, log)
			}
			var code int
			var body string
			start := time.Now()
			for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
				code, body = h.get(domain, check[0])
				if code == 200 && strings.Contains(body, check[1]) {
					break
				}
			}
			if code != 200 || !strings.Contains(body, check[1]) {
				t.Fatalf("%s%s: %d %.300s\nlogs:\n%s", domain, check[0], code, body,
					p.call(200, "GET", base+"/logs?tail=40", nil)["log"])
			}
			t.Logf("%s answers after %s", id, time.Since(start).Round(time.Second))
			p.call(200, "DELETE", base+"?volumes=1", nil)
		})
	}
}
