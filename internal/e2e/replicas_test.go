package e2e

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppReplicas: an app placed on a second server gets a route there
// (certificates through DNS-01), a health check per server, automatic DNS
// records that follow those checks, and deployments on both servers.
func TestAppReplicas(t *testing.T) {
	p := startPanel(t)
	h1, h2, h3 := startHost(t, p), startHost(t, p), startHost(t, p)
	for i, h := range []*host{h1, h2, h3} {
		if i < 2 {
			if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, h.id); err != nil {
				t.Fatal(err)
			}
		}
		p.call(200, "PUT", "/api/v1/servers/"+h.id+"/public-ips", map[string]any{"ips": []string{"192.0.2." + strconv.Itoa(11+i)}})
	}
	sid := func(h *host) int64 { return mustAtoi(h.id) }
	zid := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})["zone"].(map[string]any)["id"])

	app := func(name string, domains []string, extra map[string]any) string {
		body := map[string]any{"server_id": sid(h1), "name": name, "image": "nginx:1.27", "port": 80, "domains": domains, "tls": "internal"}
		for k, v := range extra {
			body[k] = v
		}
		return jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", body)["app"].(map[string]any)["id"])
	}
	web := app("web", []string{"web.example.test"}, nil)
	replicas := func(id string) string { return "/api/v1/apps/" + id + "/replicas" }

	// What cannot run on several servers.
	p.call(http.StatusUnprocessableEntity, "POST", replicas(app("bare", nil, nil)), map[string]any{"server_id": sid(h2)})
	p.call(http.StatusUnprocessableEntity, "POST", replicas(app("outside", []string{"web.example.org"}, nil)), map[string]any{"server_id": sid(h2)})
	p.call(http.StatusUnprocessableEntity, "POST", replicas(app("stateful", []string{"data.example.test"},
		map[string]any{"volumes": []map[string]string{{"name": "data", "path": "/data"}}})), map[string]any{"server_id": sid(h2)})
	p.call(http.StatusUnprocessableEntity, "POST", replicas(web), map[string]any{"server_id": sid(h3)}) // no Docker
	p.call(http.StatusUnprocessableEntity, "POST", replicas(web), map[string]any{"server_id": 999})
	p.call(http.StatusConflict, "POST", replicas(web), map[string]any{"server_id": sid(h1)})

	r := p.call(http.StatusCreated, "POST", replicas(web), map[string]any{"server_id": sid(h2)})
	if jsonString(r["app"].(map[string]any)["replicas"]) != "["+h2.id+"]" {
		t.Fatalf("app = %v", r["app"])
	}
	p.call(http.StatusConflict, "POST", replicas(web), map[string]any{"server_id": sid(h2)})

	// A health check per server, through its proxy with the app's domain.
	got := p.call(200, "GET", "/api/v1/apps/"+web, nil)
	placements := got["placements"].([]any)
	if len(placements) != 2 || placements[0].(map[string]any)["primary"] != true {
		t.Fatalf("placements = %v", placements)
	}
	checks := p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any)
	if len(checks) != 2 {
		t.Fatalf("checks = %v", checks)
	}
	for i, h := range []*host{h1, h2} {
		c := checks[i].(map[string]any)
		if c["kind"] != "https" || c["host"] != "192.0.2."+strconv.Itoa(11+i) || int(c["port"].(float64)) != h.httpsPort ||
			c["host_header"] != "web.example.test" || c["insecure"] != true ||
			jsonID(c["id"]) != jsonID(placements[i].(map[string]any)["health_check"]) {
			t.Fatalf("check %d = %v", i, c)
		}
	}

	// webRoute is the route of the app on a server (nil if none).
	webRoute := func(h *host) map[string]any {
		for _, r := range p.call(200, "GET", "/api/v1/servers/"+h.id+"/routes", nil)["routes"].([]any) {
			if r := r.(map[string]any); r["app_id"] != nil && jsonID(r["app_id"]) == web {
				return r
			}
		}
		return nil
	}
	// Both servers serve the domain and take its certificate through DNS-01.
	// (Caddy is one per process: of several hosts in a test only the last
	// one's proxy runs, so the routes are checked, not their application.)
	for _, h := range []*host{h1, h2} {
		if r := webRoute(h); r == nil || r["dns_challenge"] != true || jsonString(r["domains"]) != `["web.example.test"]` {
			t.Fatalf("route on %s = %v", h.id, r)
		}
	}
	// DNS points at both servers, each address following its server's check.
	follows := map[string]string{} // address -> check of the web records
	for _, a := range p.call(200, "GET", "/api/v1/dns/zones/"+zid, nil)["auto"].([]any) {
		if a := a.(map[string]any); a["name"] == "web" && a["ttl"].(float64) == 60 && a["health_check"] != nil {
			follows[a["value"].(string)] = jsonID(a["health_check"])
		}
	}
	for i := range 2 {
		if c := jsonID(checks[i].(map[string]any)["id"]); follows["192.0.2."+strconv.Itoa(11+i)] != c {
			t.Fatalf("auto records follow %v, want check %s for server %d", follows, c, i+1)
		}
	}

	// Status and logs of one server; not of a server the app is not on.
	p.call(http.StatusUnprocessableEntity, "GET", "/api/v1/apps/"+web+"/status?server="+h3.id, nil)

	// A deployment runs server by server; this one stops at the first
	// (no Docker here), so the replica keeps what it ran.
	did := jsonID(p.call(http.StatusAccepted, "POST", "/api/v1/apps/"+web+"/deploy", map[string]any{})["deployment"].(map[string]any)["id"])
	var dep map[string]any
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if dep = p.call(200, "GET", "/api/v1/apps/"+web+"/deployments/"+did, nil)["deployment"].(map[string]any); dep["status"] != "running" {
			break
		}
	}
	h1name := p.call(200, "GET", "/api/v1/servers/"+h1.id, nil)["server"].(map[string]any)["name"].(string)
	h2name := p.call(200, "GET", "/api/v1/servers/"+h2.id, nil)["server"].(map[string]any)["name"].(string)
	if dep["status"] != "failed" || !strings.HasPrefix(dep["log"].(string), "=== "+h1name+" ===\n") ||
		!strings.HasPrefix(dep["error"].(string), h1name+": ") || strings.Contains(dep["log"].(string), h2name) {
		t.Fatalf("deployment = %v", dep)
	}

	// A replicated app keeps no volumes and links no database.
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/apps/"+web, map[string]any{"image": "nginx:1.27", "port": 80,
		"volumes": []map[string]string{{"name": "data", "path": "/data"}}})
	db := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid(h1), "name": "pg",
		"source": "database", "engine": "postgres"})["app"].(map[string]any)["id"])
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+db+"/links", map[string]any{"app_id": mustAtoi(web)})
	// Its domains stay in zones under DNS.
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/apps/"+web+"/domains", map[string]any{"domains": []string{"web.example.org"}})
	p.call(200, "PUT", "/api/v1/apps/"+web+"/domains", map[string]any{"domains": []string{"web.example.test", "www.example.test"}})
	if r := webRoute(h2); r == nil || len(r["domains"].([]any)) != 2 {
		t.Fatalf("replica route = %v", r)
	}

	// Viewers see, admins change.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(200, "GET", "/api/v1/apps/"+web, nil)
	viewer.call(http.StatusForbidden, "DELETE", replicas(web)+"/"+h2.id, nil)

	// Taking the replica away removes its route and both checks.
	p.call(http.StatusNotFound, "DELETE", replicas(web)+"/"+h1.id, nil)
	p.call(200, "DELETE", replicas(web)+"/"+h2.id, nil)
	if n := len(p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any)); n != 0 {
		t.Fatalf("%d checks left", n)
	}
	if r := webRoute(h2); r != nil {
		t.Fatalf("replica route kept: %v", r)
	}
	if r := webRoute(h1); r == nil || r["dns_challenge"] == true {
		t.Fatalf("primary route = %v", r)
	}

	// Deleting a replicated app removes it everywhere.
	p.call(http.StatusCreated, "POST", replicas(web), map[string]any{"server_id": sid(h2)})
	p.call(200, "DELETE", "/api/v1/apps/"+web, nil)
	if n := len(p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any)); n != 0 {
		t.Fatalf("%d checks left after delete", n)
	}
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(action) FROM (SELECT action FROM audit_log WHERE action LIKE 'app.replica%' ORDER BY id)`).Scan(&audit)
	if audit != "app.replica_added,app.replica_removed,app.replica_added" {
		t.Fatalf("audit: %s", audit)
	}
}
