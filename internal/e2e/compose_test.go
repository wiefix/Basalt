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

// TestComposeAppAPI needs no Docker: validation, storage and roles.
func TestComposeAppAPI(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p) // no Docker: deployments fail on the agent
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	create := func(want int, name, file string, extra map[string]any) map[string]any {
		body := map[string]any{"server_id": sid, "name": name, "source": "compose", "compose": file}
		for k, v := range extra {
			body[k] = v
		}
		return p.call(want, "POST", "/api/v1/apps", body)
	}
	for _, bad := range []string{
		"services:\n  a:\n    image: nginx:1.27\n    volumes: [\"/:/host\"]\n",
		"services:\n  a:\n    image: nginx:1.27\n    privileged: true\n",
		"services:\n  a:\n    build: .\n",
		"not: [valid",
	} {
		create(http.StatusUnprocessableEntity, "bad", bad, nil)
	}
	const file = `services:
  web:
    image: traefik/whoami:v1.10
    ports: ["8080:80"]
    environment:
      WHOAMI_NAME: ${GREETING}
  cache:
    image: redis:7.4-alpine
    environment:
      REDIS_PASSWORD: hunter2-in-compose
`
	create(http.StatusUnprocessableEntity, "x", file, map[string]any{"service": "nope"})
	r := create(http.StatusCreated, "stack", file, map[string]any{"domains": []string{"stack.localhost"}, "tls": "internal"})
	app := r["app"].(map[string]any)
	aid := jsonID(app["id"])
	sum := app["compose"].(map[string]any)
	if app["source"] != "compose" || app["service"] != "web" || app["port"] != float64(80) || len(sum["services"].([]any)) != 2 {
		t.Fatalf("app = %v", app)
	}
	if w := fmt.Sprint(r["warnings"]); !strings.Contains(w, "GREETING") || !strings.Contains(w, "ports are not published") {
		t.Fatalf("warnings = %v", r["warnings"])
	}
	var sealed []byte
	p.db.R.QueryRow(`SELECT compose FROM apps WHERE id = ?`, mustAtoi(aid)).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), "hunter2") {
		t.Fatal("compose file stored in clear")
	}
	base := "/api/v1/apps/" + aid
	got := p.call(200, "GET", base+"/compose", nil)
	if got["compose"] != file || got["service"] != "web" {
		t.Fatalf("compose = %v", got)
	}
	// The environment feeds ${VARIABLES}: the warning goes away.
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"GREETING": "hello"}})
	if w := p.call(200, "GET", base+"/compose", nil)["warnings"].([]any); len(w) != 1 {
		t.Fatalf("warnings with env = %v", w)
	}
	// Settings of a compose app come from its file.
	p.call(http.StatusUnprocessableEntity, "PUT", base, map[string]any{"image": "nginx:1.27", "port": 80})
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/compose", map[string]any{"compose": file, "service": "cache", "port": 0})
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/compose", map[string]any{"compose": "services:\n  a:\n    image: x:1\n    pid: host\n"})
	upd := p.call(200, "PUT", base+"/compose", map[string]any{"compose": file, "service": "cache", "port": 6379})
	if a := upd["app"].(map[string]any); a["service"] != "cache" || a["port"] != float64(6379) {
		t.Fatalf("updated = %v", a)
	}
	p.call(http.StatusUnprocessableEntity, "GET", base+"/logs?service=../x", nil)

	// Deploying reaches the agent (which has no Docker here); the stored log
	// starts with what the panel noticed.
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	d, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if d["status"] != "failed" || !strings.Contains(log, "Docker is not available") {
		t.Fatalf("deployment: %v %q", d, log)
	}
	p.call(http.StatusUnprocessableEntity, "POST", base+"/deploy", map[string]any{"rollback_to": mustAtoi(jsonID(dep["id"]))})

	// Viewers see the services but not the file.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer"})
	v := *p
	v.hc = &http.Client{Jar: newJar()}
	v.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	if s := jsonString(v.call(200, "GET", base, nil)); strings.Contains(s, "hunter2") || !strings.Contains(s, "redis:7.4-alpine") {
		t.Fatalf("viewer view = %s", s)
	}
	v.call(http.StatusForbidden, "GET", base+"/compose", nil)
	v.call(http.StatusForbidden, "PUT", base+"/compose", map[string]any{"compose": file})
	p.call(http.StatusNotFound, "GET", "/api/v1/apps/"+jsonID(create(http.StatusCreated, "img", "", map[string]any{"source": "image", "image": "nginx:1.27", "port": 80})["app"].(map[string]any)["id"])+"/compose", nil)
}

// TestComposeAppWithDocker deploys a stack to the local Docker daemon. Opt-in.
func TestComposeAppWithDocker(t *testing.T) {
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

	// web needs a healthy cache and a finished migration; cache keeps its
	// data in a volume.
	stack := func(webCmd, cacheCmd string) string {
		return `services:
  web:
    image: traefik/whoami:v1.10
    command: ` + webCmd + `
    environment:
      WHOAMI_NAME: ${GREETING}
    depends_on:
      cache: {condition: service_healthy}
      migrate: {condition: service_completed_successfully}
  cache:
    image: redis:7.4-alpine
    command: ` + cacheCmd + `
    volumes: ["data:/data"]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 1s
      retries: 30
  migrate:
    image: busybox:1.37
    command: ["sh", "-c", "echo migrating; sleep 1; echo done"]
volumes:
  data:
`
	}
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "stack", "source": "compose",
		"compose": stack(`["--port", "80"]`, `["redis-server", "--save", "1", "1"]`),
		"service": "web", "port": 80, "domains": []string{"stack.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, appLabels(aid))
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, appNet(aid))
		dc.VolumeRemove(ctx, appNet(aid)+"-data")
	})
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"GREETING": "one"}})
	deploy := func(body map[string]any) (map[string]any, string) {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", base+"/deploy", body)["deployment"].(map[string]any)
		return followDeployment(t, p, base, jsonID(dep["id"]))
	}
	served := func(name string) func() bool {
		return func() bool { _, body := h.get("stack.localhost", "/"); return strings.Contains(body, "Name: "+name) }
	}
	containerOf := func(service string) string {
		list, _ := dc.ContainerList(context.Background(), appLabels(aid, "basalt.service", service))
		for _, c := range list {
			if c.State == "running" {
				return c.ID
			}
		}
		return ""
	}

	d1, log := deploy(map[string]any{})
	if d1["status"] != "succeeded" {
		t.Fatalf("first deployment: %v\n%s", d1, log)
	}
	if i, j, k := strings.Index(log, "Starting service cache"), strings.Index(log, "Service migrate finished"), strings.Index(log, "Starting service web"); i < 0 || j < 0 || k < 0 || !(i < k && j < k) {
		t.Fatalf("services did not start in dependency order:\n%s", log)
	}
	t.Logf("first deployment:\n%s", log)
	eventually(t, "stack served", served("one"))
	st := p.call(200, "GET", base+"/status", nil)["status"].(map[string]any)
	if svcs := st["services"].([]any); len(svcs) != 3 || st["state"] != "running" {
		t.Fatalf("status = %v", st)
	}
	logs := p.call(200, "GET", base+"/logs?service=migrate&tail=10", nil)["log"].(string)
	if !strings.Contains(logs, "migrating") {
		t.Fatalf("migrate logs = %q", logs)
	}
	cache1 := containerOf("cache")

	// Changing only web's environment: cache is kept, web is replaced
	// without dropping requests.
	var ok, failed atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if code, _ := h.get("stack.localhost", "/"); code == 200 {
				ok.Add(1)
			} else {
				failed.Add(1)
			}
		}
	}()
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"GREETING": "two"}})
	d2, log := deploy(map[string]any{})
	close(stop)
	<-done
	if d2["status"] != "succeeded" || !strings.Contains(log, "Service cache is unchanged") || containerOf("cache") != cache1 {
		t.Fatalf("second deployment: %v\n%s", d2, log)
	}
	eventually(t, "new web served", served("two"))
	if failed.Load() != 0 || ok.Load() == 0 {
		t.Fatalf("%d of %d requests failed while web was replaced", failed.Load(), ok.Load()+failed.Load())
	}

	// A broken change to both services: cache (with a volume) is stopped and
	// replaced, then web fails; everything is put back.
	p.call(200, "PUT", base+"/compose", map[string]any{
		"compose": stack(`["--no-such-flag"]`, `["redis-server", "--save", "5", "1"]`), "service": "web", "port": 80})
	d3, log := deploy(map[string]any{})
	t.Logf("broken deployment:\n%s", log)
	if d3["status"] != "failed" || !strings.Contains(log, "Stopping service cache") || !strings.Contains(log, "Restored the previous containers") {
		t.Fatalf("broken deployment: %v\n%s", d3, log)
	}
	if containerOf("cache") != cache1 || !served("two")() {
		t.Fatal("the failed deployment did not restore the previous containers")
	}

	// Roll back to the first deployment: its compose file and images.
	rb, log := deploy(map[string]any{"rollback_to": mustAtoi(jsonID(d1["id"]))})
	if rb["status"] != "succeeded" {
		t.Fatalf("rollback: %v\n%s", rb, log)
	}
	t.Logf("rollback:\n%s", log)
	eventually(t, "rolled back web served", served("two")) // env stays current
	if digests, _ := rb["digests"].(map[string]any); len(digests) != 3 || !strings.Contains(fmt.Sprint(digests["web"]), "@sha256:") {
		t.Fatalf("digests = %v", rb["digests"])
	}

	// Removing a service from the file removes its container.
	p.call(200, "PUT", base+"/compose", map[string]any{"compose": `services:
  web:
    image: traefik/whoami:v1.10
    environment:
      WHOAMI_NAME: ${GREETING}
`, "service": "web", "port": 80})
	d5, log := deploy(map[string]any{})
	if d5["status"] != "succeeded" || !strings.Contains(log, "Removing service cache") {
		t.Fatalf("shrunk deployment: %v\n%s", d5, log)
	}
	time.Sleep(200 * time.Millisecond)
	if containerOf("cache") != "" {
		t.Fatal("container of a removed service left running")
	}

	p.call(200, "DELETE", base+"?volumes=1", nil)
	if list, _ := dc.ContainerList(context.Background(), appLabels(aid)); len(list) != 0 {
		t.Fatalf("%d containers left", len(list))
	}
	if vols, _ := dc.VolumeList(context.Background(), appLabels(aid)); len(vols) != 0 {
		t.Fatalf("volumes left: %v", vols)
	}
}
