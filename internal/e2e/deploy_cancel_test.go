package e2e

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/docker"
)

// TestCancelDeploymentWithDocker: a canceled deployment removes its new
// container and the app keeps serving the previous one.
func TestCancelDeploymentWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	p := startPanel(t)
	h := startHostWith(t, p, sock)
	dc := docker.New(sock)
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "cancel", "image": "traefik/whoami:v1.10", "port": 80,
		"domains": []string{"cancel.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	var deployments []string // only this test's containers are removed
	t.Cleanup(func() {
		ctx := context.Background()
		for _, dep := range deployments {
			list, _ := dc.ContainerList(ctx, map[string]string{"basalt.app": aid, "basalt.deployment": dep})
			for _, c := range list {
				dc.ContainerRemove(ctx, c.ID, true)
			}
		}
	})

	first := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	deployments = append(deployments, jsonID(first["id"]))
	if d, log := followDeployment(t, p, base, jsonID(first["id"])); d["status"] != "succeeded" {
		t.Fatalf("first deployment: %v\n%s", d, log)
	}
	eventually(t, "app served", func() bool { code, _ := h.get("cancel.localhost", "/"); return code == 200 })
	p.call(http.StatusConflict, "POST", base+"/deployments/"+jsonID(first["id"])+"/cancel", nil)
	p.call(http.StatusNotFound, "POST", base+"/deployments/999999/cancel", nil)

	// Nothing listens on port 81: the new container never becomes ready.
	p.call(200, "PUT", base, map[string]any{"image": "traefik/whoami:v1.10", "port": 81, "volumes": []any{}})
	second := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	did := jsonID(second["id"])
	deployments = append(deployments, did)
	ch, stop := p.events(base + "/deployments/" + did + "/stream")
	waitLog(t, ch, "Starting container")
	stop()

	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(http.StatusForbidden, "POST", base+"/deployments/"+did+"/cancel", nil)

	p.call(http.StatusAccepted, "POST", base+"/deployments/"+did+"/cancel", nil)
	d, log := followDeployment(t, p, base, did)
	if d["status"] != "failed" || !strings.HasPrefix(d["error"].(string), "canceled by O") {
		t.Fatalf("canceled deployment: %v\n%s", d, log)
	}
	got := p.call(200, "GET", base+"/deployments/"+did, nil)["deployment"].(map[string]any)["log"].(string)
	if !strings.Contains(got, "deployment canceled") {
		t.Fatalf("log: %s", got)
	}
	// (Filtered by deployment: a dev panel on the same Docker may use the
	// same app ID.)
	if list, _ := dc.ContainerList(context.Background(), map[string]string{"basalt.app": aid, "basalt.deployment": did}); len(list) != 0 {
		t.Fatalf("container of the canceled deployment left: %+v", list)
	}
	if list, _ := dc.ContainerList(context.Background(), map[string]string{"basalt.app": aid, "basalt.deployment": jsonID(first["id"])}); len(list) != 1 {
		t.Fatalf("previous container: %+v", list)
	}
	if code, _ := h.get("cancel.localhost", "/"); code != 200 {
		t.Fatalf("previous deployment not served: %d", code)
	}
	var audit string
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'app.deploy_canceled'`).Scan(&audit)
	if audit != "1" {
		t.Fatalf("audit: %s", audit)
	}
}
