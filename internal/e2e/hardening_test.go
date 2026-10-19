package e2e

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

// TestHardeningWithDocker checks the Docker security report, a read-only
// root filesystem and container metrics against the local daemon. Opt-in.
func TestHardeningWithDocker(t *testing.T) {
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

	sec, _ := p.server(h.id)["docker_security"].(map[string]any)
	if sec == nil || sec["seccomp"] == "" || sec["seccomp"] == nil {
		t.Fatalf("docker_security = %v", p.server(h.id)["docker_security"])
	}
	t.Logf("docker security: %v", sec)

	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "ro", "image": "traefik/whoami:v1.10", "port": 80, "read_only": true,
		"domains": []string{"ro.localhost"}, "tls": "internal",
	})
	app := r["app"].(map[string]any)
	if app["read_only"] != true {
		t.Fatalf("app = %v", app)
	}
	id := jsonID(app["id"])
	base := "/api/v1/apps/" + id
	t.Cleanup(func() { p.call(200, "DELETE", base, nil) })
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	if d, log := followDeployment(t, p, base, jsonID(dep["id"])); d["status"] != "succeeded" {
		t.Fatalf("deployment: %v\n%s", d, log)
	}
	list, _ := dc.ContainerList(context.Background(), appLabels(id))
	if len(list) != 1 {
		t.Fatalf("%d containers", len(list))
	}
	info, err := dc.ContainerInspect(context.Background(), list[0].ID)
	if err != nil || !info.HostConfig.ReadonlyRootfs || len(info.HostConfig.Tmpfs) != 2 {
		t.Fatalf("host config = %+v, %v", info, err)
	}
	eventually(t, "app served", func() bool { code, _ := h.get("ro.localhost", "/"); return code == 200 })

	// Samples arrive with the server's metrics; CPU needs two.
	deadline := time.Now().Add(20 * time.Second)
	for {
		m := p.call(200, "GET", base+"/metrics?minutes=5", nil)["metrics"].([]any)
		if len(m) >= 3 {
			last := m[len(m)-1].(map[string]any)
			if last["mem_used"].(float64) <= 0 || last["mem_limit"].(float64) <= 0 {
				t.Fatalf("sample = %v", last)
			}
			t.Logf("last sample: %v", last)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics = %v", m)
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.call(http.StatusUnprocessableEntity, "GET", base+"/metrics?minutes=0", nil)
}
