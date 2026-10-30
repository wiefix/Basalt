package e2e

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/docker"
)

const routesStack = `services:
  web:
    image: traefik/whoami:v1.10
    environment:
      WHOAMI_NAME: web
    ports: ["80"]
  api:
    image: traefik/whoami:v1.10
    command: ["--port", "8081"]
    environment:
      WHOAMI_NAME: ${API_NAME}
    ports: ["8081"]
    depends_on:
      migrate: {condition: service_completed_successfully}
  migrate:
    image: busybox:1.37
    command: ["true"]
`

// TestComposeServiceRoutes needs no Docker: other services of a compose app
// with domains of their own, their validation and what the proxy gets.
func TestComposeServiceRoutes(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	app := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": sid, "name": "stack", "source": "compose", "compose": routesStack,
		"service": "web", "port": 80, "domains": []string{"stack.localhost"}, "tls": "internal",
	})["app"].(map[string]any)
	base := "/api/v1/apps/" + jsonID(app["id"])
	if r, ok := app["routes"].([]any); !ok || len(r) != 0 {
		t.Fatalf("routes of a new compose app = %v", app["routes"])
	}
	set := func(want int, routes ...map[string]any) map[string]any {
		t.Helper()
		if routes == nil {
			routes = []map[string]any{}
		}
		return p.call(want, "PUT", base+"/routes", map[string]any{"routes": routes})
	}
	route := func(service string, port int, domains ...string) map[string]any {
		return map[string]any{"service": service, "port": port, "domains": domains}
	}
	for name, bad := range map[string][]map[string]any{
		"unknown service":    {route("nope", 80, "x.localhost")},
		"published service":  {route("web", 80, "x.localhost")},
		"job":                {route("migrate", 80, "x.localhost")},
		"no domains":         {route("api", 8081)},
		"published's domain": {route("api", 8081, "stack.localhost")},
		"listed twice":       {route("api", 8081, "a.localhost"), route("api", 8081, "b.localhost")},
		"bad port":           {route("api", 70000, "a.localhost")},
		"bad domain":         {route("api", 8081, "not a domain")},
	} {
		t.Run(name, func(t *testing.T) { set(http.StatusUnprocessableEntity, bad...) })
	}

	// The port defaults to the first one the file mentions.
	got := set(200, route("api", 0, "API.localhost"))["app"].(map[string]any)
	if r := got["routes"].([]any); len(r) != 1 || jsonString(r[0]) != `{"domains":["api.localhost"],"port":8081,"service":"api"}` {
		t.Fatalf("routes = %v", got["routes"])
	}
	d, err := p.db.DesiredProxy(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Routes) != 1 || len(d.Routes[0].Services) != 1 || d.Routes[0].Services[0].Service != "api" ||
		strings.Join(d.Routes[0].Domains, ",") != "stack.localhost" {
		t.Fatalf("desired proxy = %+v", d.Routes)
	}

	// The published service's domains and the others' are kept apart.
	p.call(200, "PUT", base+"/domains", map[string]any{"domains": []string{"stack.localhost", "www.stack.localhost"}})
	p.call(http.StatusConflict, "PUT", base+"/domains", map[string]any{"domains": []string{"api.localhost"}})
	got = p.call(200, "GET", base, nil)["app"].(map[string]any)
	if jsonString(got["domains"]) != `["stack.localhost","www.stack.localhost"]` || len(got["routes"].([]any)) != 1 {
		t.Fatalf("app after domains change = %v / %v", got["domains"], got["routes"])
	}

	// A compose file without the service, or publishing it, is refused.
	without := strings.Replace(routesStack, "  api:", "  api2:", 1)
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/compose", map[string]any{"compose": without, "service": "web", "port": 80})
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/compose", map[string]any{"compose": routesStack, "service": "api", "port": 8081})

	// Viewers see the routes but cannot change them; other apps have none.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer"})
	v := *p
	v.hc = &http.Client{Jar: newJar()}
	v.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	if s := jsonString(v.call(200, "GET", base, nil)); !strings.Contains(s, "api.localhost") {
		t.Fatalf("viewer view = %s", s)
	}
	v.call(http.StatusForbidden, "PUT", base+"/routes", map[string]any{"routes": []any{}})
	img := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "img", "source": "image", "image": "nginx:1.27", "port": 80})
	p.call(http.StatusNotFound, "PUT", "/api/v1/apps/"+jsonID(img["app"].(map[string]any)["id"])+"/routes", map[string]any{"routes": []any{}})

	// Clearing them frees the domain.
	set(200)
	p.call(200, "PUT", base+"/domains", map[string]any{"domains": []string{"stack.localhost", "api.localhost"}})
}

// TestComposeServiceRoutesWithDocker serves two services of one compose
// app on their own domains. Opt-in.
func TestComposeServiceRoutesWithDocker(t *testing.T) {
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
	aid := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "routes", "source": "compose", "compose": routesStack,
		"service": "web", "port": 80, "domains": []string{"web.localhost"}, "tls": "internal",
	})["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, appLabels(aid))
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, appNet(aid))
	})
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"API_NAME": "api-one"}})
	p.call(200, "PUT", base+"/routes", map[string]any{"routes": []map[string]any{{"service": "api", "domains": []string{"api.localhost"}}}})
	deploy := func() {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
		if d, log := followDeployment(t, p, base, jsonID(dep["id"])); d["status"] != "succeeded" {
			t.Fatalf("deployment: %v\n%s", d, log)
		}
	}
	served := func(host, name string) func() bool {
		return func() bool { _, body := h.get(host, "/"); return strings.Contains(body, "Name: "+name) }
	}
	deploy()
	eventually(t, "web served", served("web.localhost", "web"))
	eventually(t, "api served on its own domain", served("api.localhost", "api-one"))

	// A new api container takes over its domain; web is unchanged.
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"API_NAME": "api-two"}})
	deploy()
	eventually(t, "new api served", served("api.localhost", "api-two"))
	if !served("web.localhost", "web")() {
		t.Fatal("web no longer served")
	}
}
