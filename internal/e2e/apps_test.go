package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

// TestAppValidationAndRoles needs no Docker: it checks what the panel
// refuses before anything reaches an agent.
func TestAppValidationAndRoles(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p) // no Docker on this host
	sid := mustAtoi(h.id)
	create := func(want int, body map[string]any) map[string]any {
		b := map[string]any{"server_id": sid, "name": "web", "image": "nginx:1.27", "port": 80}
		for k, v := range body {
			b[k] = v
		}
		return p.call(want, "POST", "/api/v1/apps", b)
	}
	create(http.StatusUnprocessableEntity, nil) // Docker missing on the server
	for _, bad := range []map[string]any{
		{"image": "nginx; rm -rf /"},
		{"image": "Nginx"},
		{"port": 70000},
		{"port": 0, "domains": []string{"a.example.com"}},
		{"volumes": []map[string]string{{"name": "data", "path": "relative"}}},
		{"volumes": []map[string]string{{"name": "../x", "path": "/data"}}},
		{"domains": []string{"*.example.com"}},
	} {
		create(http.StatusUnprocessableEntity, bad)
	}

	// Pretend the server has Docker to create the record.
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	r := create(http.StatusCreated, map[string]any{"domains": []string{"app.localhost"}, "tls": "internal"})
	aid := jsonID(r["app"].(map[string]any)["id"])
	create(http.StatusConflict, map[string]any{"name": "other", "domains": []string{"app.localhost"}})
	create(http.StatusConflict, nil) // name taken

	base := "/api/v1/apps/" + aid
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/env", map[string]any{"env": map[string]string{"1BAD": "x"}})
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"SECRET_KEY": "s3cr3t", "MODE": "prod"}})
	if env := p.call(200, "GET", base+"/env", nil)["env"].(map[string]any); env["SECRET_KEY"] != "s3cr3t" {
		t.Fatalf("env = %v", env)
	}
	var sealed []byte
	p.db.R.QueryRow(`SELECT env FROM apps WHERE id = ?`, mustAtoi(aid)).Scan(&sealed)
	if strings.Contains(string(sealed), "s3cr3t") {
		t.Fatal("environment stored in clear")
	}
	// The secret value must not reach the audit log.
	var details string
	p.db.R.QueryRow(`SELECT details FROM audit_log WHERE action = 'app.env_changed'`).Scan(&details)
	if strings.Contains(details, "s3cr3t") || !strings.Contains(details, "SECRET_KEY") {
		t.Fatalf("audit details = %s", details)
	}
	// Rolling back needs a successful deployment.
	p.call(http.StatusUnprocessableEntity, "POST", base+"/deploy", map[string]any{"rollback_to": 999})

	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(200, "GET", base, nil)
	viewer.call(200, "GET", base+"/deployments", nil)
	viewer.call(http.StatusForbidden, "GET", base+"/env", nil)
	viewer.call(http.StatusForbidden, "GET", base+"/logs", nil)
	viewer.call(http.StatusForbidden, "POST", base+"/deploy", map[string]any{})
	viewer.call(http.StatusForbidden, "DELETE", base, nil)

	// The app route cannot be removed as a plain route.
	routes := p.call(200, "GET", "/api/v1/servers/"+h.id+"/routes", nil)["routes"].([]any)
	if len(routes) != 1 || routes[0].(map[string]any)["app_id"] == nil {
		t.Fatalf("routes = %v", routes)
	}
	p.call(http.StatusNotFound, "DELETE", "/api/v1/servers/"+h.id+"/routes/"+jsonID(routes[0].(map[string]any)["id"]), nil)
	p.call(200, "DELETE", base, nil)
	if n := len(p.call(200, "GET", "/api/v1/servers/"+h.id+"/routes", nil)["routes"].([]any)); n != 0 {
		t.Fatalf("%d routes left after deleting the app", n)
	}
}

// TestAppsWithDocker runs the whole chain against the local Docker daemon:
// panel -> agent -> Docker -> proxy -> container. Opt-in (pulls images).
func TestAppsWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	p := startPanel(t)
	h := startHostWith(t, p, sock)
	sid := mustAtoi(h.id)
	if p.server(h.id)["docker"] == "" {
		t.Fatal("agent did not report Docker")
	}
	dc := docker.New(sock)

	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": sid, "name": "whoami", "image": "traefik/whoami:v1.10", "port": 80,
		"domains": []string{"whoami.localhost"}, "tls": "internal",
		"volumes": []map[string]string{{"name": "data", "path": "/data"}},
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, map[string]string{"basalt.app": aid})
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, "basalt-app-"+aid)
		dc.VolumeRemove(ctx, "basalt-app-"+aid+"-data")
	})

	// Before any deployment the domain answers 503.
	eventually(t, "503 before deploy", func() bool { code, _ := h.get("whoami.localhost", "/"); return code == 503 })

	deploy := func(body map[string]any) map[string]any {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", base+"/deploy", body)["deployment"].(map[string]any)
		did := jsonID(dep["id"])
		var d map[string]any
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			d = p.call(200, "GET", base+"/deployments/"+did, nil)["deployment"].(map[string]any)
			if d["status"] != "running" {
				return d
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("deployment %s did not finish", did)
		return nil
	}

	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"WHOAMI_NAME": "first"}})
	d1 := deploy(map[string]any{})
	if d1["status"] != "succeeded" || !strings.Contains(d1["image_digest"].(string), "@sha256:") {
		t.Fatalf("first deployment: %v", d1)
	}
	t.Logf("deploy log:\n%s", d1["log"])
	eventually(t, "app served", func() bool {
		code, body := h.get("whoami.localhost", "/")
		return code == 200 && strings.Contains(body, "Name: first")
	})
	st := p.call(200, "GET", base+"/status", nil)["status"].(map[string]any)
	if st["state"] != "running" || jsonID(st["deployment_id"]) != jsonID(d1["id"]) {
		t.Fatalf("status = %v", st)
	}

	// A second deployment replaces the container without dropping requests.
	var served, failed atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if code, body := h.get("whoami.localhost", "/"); code == 200 {
				served.Add(1)
			} else {
				failed.Add(1)
				firstFailure.CompareAndSwap(nil, fmt.Sprintf("%d %q at %s", code, body, time.Now().Format("15:04:05.000")))
			}
		}
	}()
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"WHOAMI_NAME": "second"}})
	if d := deploy(map[string]any{}); d["status"] != "succeeded" {
		t.Fatalf("second deployment: %v", d)
	}
	eventually(t, "new container serves", func() bool {
		_, body := h.get("whoami.localhost", "/")
		return strings.Contains(body, "Name: second")
	})
	close(stop)
	<-done
	t.Logf("during redeploy: %d served, %d failed", served.Load(), failed.Load())
	if failed.Load() != 0 || served.Load() == 0 {
		t.Fatalf("requests failed during a zero-downtime deployment: %d of %d; first: %v",
			failed.Load(), served.Load()+failed.Load(), firstFailure.Load())
	}
	list, _ := dc.ContainerList(context.Background(), map[string]string{"basalt.app": aid})
	if len(list) != 1 {
		t.Fatalf("%d containers after redeploy; the old one must be removed", len(list))
	}

	// A broken image fails the deployment; the running version keeps serving.
	p.call(200, "PUT", base, map[string]any{"image": "traefik/whoami:no-such-tag", "port": 80,
		"volumes": []map[string]string{{"name": "data", "path": "/data"}}})
	if d := deploy(map[string]any{}); d["status"] != "failed" || !strings.Contains(fmt.Sprint(d["log"]), "pull") {
		t.Fatalf("broken deployment: %v", d)
	}
	if _, body := h.get("whoami.localhost", "/"); !strings.Contains(body, "Name: second") {
		t.Fatal("a failed deployment took the app down")
	}

	// Roll back to the first deployment's image, pinned by digest.
	rb := deploy(map[string]any{"rollback_to": mustAtoi(jsonID(d1["id"]))})
	if rb["status"] != "succeeded" || !strings.Contains(rb["image"].(string), "@sha256:") {
		t.Fatalf("rollback: %v", rb)
	}

	logs := p.call(200, "GET", base+"/logs?tail=50", nil)["log"].(string)
	if !strings.Contains(logs, "Starting up on port 80") {
		t.Fatalf("logs = %q", logs)
	}

	// Deleting the app (keeping data) removes containers and the network.
	p.call(200, "DELETE", base, nil)
	list, _ = dc.ContainerList(context.Background(), map[string]string{"basalt.app": aid})
	if len(list) != 0 {
		t.Fatalf("containers left: %d", len(list))
	}
	if _, err := dc.NetworkInspect(context.Background(), "basalt-app-"+aid); err == nil {
		t.Fatal("network left behind")
	}
	if vols, _ := dc.VolumeList(context.Background(), map[string]string{"basalt.app": aid}); len(vols) != 1 {
		t.Fatalf("data volume must be kept by default: %v", vols)
	}
	// Without a route the name gets no certificate either: any non-app answer.
	eventually(t, "domain no longer served", func() bool {
		code, _ := h.get("whoami.localhost", "/")
		return code != 200 && code != 503
	})
}
