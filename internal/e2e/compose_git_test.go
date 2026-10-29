package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/docker"
)

// composeRepo is a repository whose compose file builds web from its
// Dockerfile (the name it greets with is a build argument) and runs a job
// from a ready image.
func composeRepo(name string) map[string]string {
	return map[string]string{
		"compose.yaml": `services:
  web:
    build:
      context: ./web
      args:
        NAME: ` + name + `
    depends_on:
      migrate: {condition: service_completed_successfully}
    expose: ["80"]
  migrate:
    image: busybox:1.37
    command: ["sh", "-c", "echo migrated"]
`,
		"web/Dockerfile": "FROM traefik/whoami:v1.10\nARG NAME\nENV WHOAMI_NAME=$NAME\n",
	}
}

// TestComposeFromGitAPI: a git app running its repository's compose file
// is created without the file (it is read from the repository), loads it
// on request through its server, publishes a service of it, and cannot
// switch to building one image.
func TestComposeFromGitAPI(t *testing.T) {
	g := startGitServer(t, composeRepo("one"))
	p := startPanel(t)
	h := startHostWith(t, p, "", func(c *agent.Config) { c.GitCAFile = g.CAFile })
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	create := func(want int, extra map[string]any) map[string]any {
		body := map[string]any{"server_id": sid, "name": "stack", "source": "git",
			"git": map[string]any{"url": g.URL, "builder": "compose"}}
		for k, v := range extra {
			body[k] = v
		}
		return p.call(want, "POST", "/api/v1/apps", body)
	}
	// Domains need the published service and its port.
	create(http.StatusUnprocessableEntity, map[string]any{"domains": []string{"stack.localhost"}})
	create(http.StatusUnprocessableEntity, map[string]any{"service": "web"})
	app := create(http.StatusCreated, map[string]any{"service": "web", "port": 80,
		"domains": []string{"stack.localhost"}, "tls": "internal"})["app"].(map[string]any)
	aid := jsonID(app["id"])
	base := "/api/v1/apps/" + aid
	git := app["git"].(map[string]any)
	if git["builder"] != "compose" || git["compose_file"] != "compose.yaml" || app["service"] != "web" || app["port"] != float64(80) {
		t.Fatalf("app = %v", app)
	}
	if c := p.call(200, "GET", base+"/compose", nil); c["compose"] != "" || c["service"] != "web" {
		t.Fatalf("compose before loading = %v", c)
	}
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/compose", map[string]any{"service": "web", "port": 80})

	r := p.call(200, "POST", base+"/compose/load", nil)
	sum := r["app"].(map[string]any)["compose"].(map[string]any)
	if commit, _ := r["commit"].(string); len(commit) != 40 || len(sum["services"].([]any)) != 2 {
		t.Fatalf("load = %v", r)
	}
	web := sum["services"].([]any)[1].(map[string]any)
	if web["name"] != "web" || web["build"] != "web/Dockerfile" || web["image"] != "" {
		t.Fatalf("web = %v", web)
	}
	c := p.call(200, "GET", base+"/compose", nil)
	if !strings.Contains(c["compose"].(string), "context: ./web") || c["error"] != nil {
		t.Fatalf("compose = %v", c)
	}
	// The published service is chosen among the file's; the file itself
	// comes from the repository only.
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/compose", map[string]any{"service": "migrate", "port": 80})
	r = p.call(200, "PUT", base+"/compose", map[string]any{"compose": "services: {}", "service": "web"})
	if a := r["app"].(map[string]any); a["port"] != float64(80) || a["service"] != "web" {
		t.Fatalf("set service = %v", r)
	}
	if c := p.call(200, "GET", base+"/compose", nil); !strings.Contains(c["compose"].(string), "context: ./web") {
		t.Fatalf("file replaced: %v", c)
	}
	// It stays a compose app.
	p.call(http.StatusUnprocessableEntity, "PUT", base, map[string]any{"git": map[string]any{"url": g.URL}, "port": 80})
	r = p.call(200, "PUT", base, map[string]any{"git": map[string]any{"url": g.URL, "builder": "compose",
		"compose_file": "deploy/compose.yaml"}})
	if a := r["app"].(map[string]any); a["port"] != float64(80) || a["git"].(map[string]any)["compose_file"] != "deploy/compose.yaml" {
		t.Fatalf("update = %v", r)
	}
	// A missing file is named, with the one the repository has.
	r = p.call(http.StatusBadGateway, "POST", base+"/compose/load", nil)
	if e := fmt.Sprint(r["error"]); !strings.Contains(e, "deploy/compose.yaml is not in the repository") {
		t.Fatalf("missing file: %v", r)
	}
	// Paths in it are relative to its directory, as in compose.
	g.commit(map[string]string{"deploy/compose.yaml": "services:\n  web:\n    build: ../web\n    expose: [\"80\"]\n"}, "deploy")
	g.push()
	r = p.call(200, "POST", base+"/compose/load", nil)
	if svcs := r["app"].(map[string]any)["compose"].(map[string]any)["services"].([]any); len(svcs) != 1 ||
		svcs[0].(map[string]any)["build"] != "web/Dockerfile" {
		t.Fatalf("compose file in a directory: %v", r)
	}
	p.call(200, "PUT", base, map[string]any{"git": map[string]any{"url": g.URL, "builder": "compose", "compose_file": "web/Dockerfile"}})
	r = p.call(http.StatusUnprocessableEntity, "POST", base+"/compose/load", nil)
	if e := fmt.Sprint(r["error"]); e == "" {
		t.Fatalf("not a compose file: %v", r)
	}
	// Previews are not offered for it.
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/previews", map[string]any{"domain": "preview.localhost"})
}

// TestComposeFromGitWithDocker: the services of a compose file from git are
// built on the server and run; a push builds exactly that commit, a
// redeploy of the same commit keeps the service as it is, and a rollback
// reuses the image built then.
func TestComposeFromGitWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	g := startGitServer(t, composeRepo("one"))
	p := startPanel(t)
	h := startHostWith(t, p, sock, func(c *agent.Config) { c.GitCAFile = g.CAFile })
	dc := docker.New(sock)
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "built-stack", "source": "git",
		"git":     map[string]any{"url": g.URL, "builder": "compose"},
		"service": "web", "port": 80, "domains": []string{"built-stack.localhost"}, "tls": "internal",
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
		for _, d := range p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any) {
			for _, img := range d.(map[string]any)["digests"].(map[string]any) {
				if s := img.(string); strings.HasPrefix(s, "basalt/") {
					dc.ImageRemove(ctx, s)
				}
			}
		}
	})
	deploy := func(body map[string]any) (map[string]any, string) {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", base+"/deploy", body)["deployment"].(map[string]any)
		return followDeployment(t, p, base, jsonID(dep["id"]))
	}
	served := func(name string) func() bool {
		return func() bool {
			_, body := h.get("built-stack.localhost", "/")
			return strings.Contains(body, "Name: "+name)
		}
	}
	webContainer := func() string {
		list, _ := dc.ContainerList(context.Background(), appLabels(aid, "basalt.service", "web"))
		for _, c := range list {
			if c.State == "running" {
				return c.ID
			}
		}
		return ""
	}

	d1, log := deploy(map[string]any{})
	if d1["status"] != "succeeded" || d1["commit_message"] != "first" || !strings.Contains(log, "Building service web from web/Dockerfile") ||
		!strings.Contains(log, "Reading compose.yaml at main") {
		t.Fatalf("first deployment: %v\n%s", d1, log)
	}
	if img := d1["digests"].(map[string]any)["web"].(string); !strings.HasPrefix(img, "basalt/app-"+aid+":web-") {
		t.Fatalf("digests = %v", d1["digests"])
	}
	eventually(t, "built stack served", served("one"))
	web1 := webContainer()

	// The same commit again: web is built again from cache, to the same
	// image, so its container stays.
	d2, log := deploy(map[string]any{})
	if d2["status"] != "succeeded" || !strings.Contains(log, "Service web is unchanged") || webContainer() != web1 {
		t.Fatalf("redeployment: %v\n%s", d2, log)
	}

	// A push builds exactly the pushed commit.
	sha3 := g.commit(composeRepo("two"), "second")
	g.push()
	secret := p.call(200, "GET", base+"/webhook", nil)["webhook"].(map[string]any)["secret"].(string)
	if code, r := p.deliver(aid, secret, "main", sha3); code != http.StatusAccepted {
		t.Fatalf("webhook: %d %v", code, r)
	}
	latest := p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any)[0].(map[string]any)
	d3, log := followDeployment(t, p, base, jsonID(latest["id"]))
	if d3["status"] != "succeeded" || d3["commit"] != sha3 {
		t.Fatalf("pushed deployment: %v\n%s", d3, log)
	}
	eventually(t, "pushed version served", served("two"))
	if c := p.call(200, "GET", base+"/compose", nil)["compose"].(string); !strings.Contains(c, "NAME: two") {
		t.Fatalf("compose file not updated from the repository: %s", c)
	}

	// Rolling back reuses the image built for the first commit.
	rb, log := deploy(map[string]any{"rollback_to": mustAtoi(jsonID(d1["id"]))})
	if rb["status"] != "succeeded" || rb["commit"] != d1["commit"] || !strings.Contains(log, "Service web: reusing the image") ||
		strings.Contains(log, "Fetching") {
		t.Fatalf("rollback: %v\n%s", rb, log)
	}
	eventually(t, "rolled back", served("one"))
}
