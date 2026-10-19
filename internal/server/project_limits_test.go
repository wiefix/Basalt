package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/store"
)

// TestProjectLimits: the panel's admins set what a project may hold; one
// more site, app, database, zone or check is refused, created or moved
// in; the sites going over the disk limit are reported once.
func TestProjectLimits(t *testing.T) {
	var mu sync.Mutex
	var titles []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct{ Title string }
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		titles = append(titles, m.Title)
		mu.Unlock()
	}))
	t.Cleanup(receiver.Close)

	e := newProjectEnv(t)
	o := e.owner
	o.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "ops", "kind": "webhook",
		"url": receiver.URL, "allow_private": true, "events": []string{EventProjectDisk}})
	admA := e.user("adm-a@example.com", store.RoleUser, map[float64]store.Role{e.a: store.RoleAdmin})
	path := "/api/v1/projects/" + num(e.a)
	all := map[string]any{"sites": 1, "apps": 1, "databases": 1, "dns_zones": 1, "health_checks": 1, "disk_mb": 1}

	// Only the panel's admins set limits, and within bounds.
	admA.want(http.StatusForbidden, "PUT", path+"/limits", all)
	o.want(http.StatusUnprocessableEntity, "PUT", path+"/limits", map[string]any{"sites": -1})
	o.want(http.StatusUnprocessableEntity, "PUT", path+"/limits", map[string]any{"apps": maxProjectLimit + 1})
	o.want(http.StatusUnprocessableEntity, "PUT", path+"/limits", map[string]any{"disk_mb": maxQuotaMB + 1})
	o.want(http.StatusNotFound, "PUT", "/api/v1/projects/999/limits", all)
	body := o.want(http.StatusOK, "PUT", path+"/limits", all).body
	if l := body["project"].(map[string]any)["limits"].(map[string]any); l["sites"] != float64(1) || l["disk_mb"] != float64(1) {
		t.Fatalf("limits: %v", l)
	}
	got := admA.want(http.StatusOK, "GET", path, nil).body
	u := got["usage"].(map[string]any)
	if u["sites"] != float64(1) || u["apps"] != float64(1) || u["databases"] != float64(1) || u["dns_zones"] != float64(1) ||
		u["health_checks"] != float64(1) || u["disk_bytes"] != float64(0) {
		t.Fatalf("usage: %v", u)
	}

	// The project is full: nothing more of any kind.
	refused := func(what string, r resp) {
		t.Helper()
		if msg, _ := r.body["error"].(string); !strings.Contains(msg, "at most 1 "+what+":") {
			t.Fatalf("%s: %q", what, msg)
		}
	}
	refused("site", admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/sites", map[string]any{"server_id": e.serverID,
		"name": "shop", "domains": []string{"shop.a.test"}, "tls": "internal", "project_id": e.a}))
	refused("app", admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID,
		"name": "api", "image": "nginx:1.27", "port": 80, "project_id": e.a}))
	refused("database", admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID,
		"name": "cache", "source": "database", "engine": "redis", "project_id": e.a}))
	refused("DNS zone", admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]any{"name": "b.test", "project_id": e.a}))
	refused("health check", admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/health-checks", map[string]any{"name": "b",
		"kind": "tcp", "host": "192.0.2.2", "port": 80, "project_id": e.a}))
	other := o.want(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{"server_id": e.serverID, "name": "other",
		"domains": []string{"other.example.com"}, "tls": "internal"}).body["site"].(map[string]any)["id"].(float64)
	refused("site", o.want(http.StatusUnprocessableEntity, "PUT", "/api/v1/sites/"+num(other)+"/project", map[string]any{"project_id": e.a}))

	// Sites over the disk limit: reported once, again after they were back
	// under.
	ctx := context.Background()
	e.srv.db.SetSiteUsage(ctx, int64(e.site), store.SiteUsage{DiskBytes: 2 << 20})
	o.want(http.StatusOK, "PUT", path+"/limits", all)
	o.want(http.StatusOK, "PUT", path+"/limits", all) // starts over: alerts again
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(titles)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disk alerts: %v", titles)
		}
	}
	pr, _ := e.srv.db.Project(ctx, int64(e.a))
	if !pr.DiskAlerted {
		t.Fatal("disk alert not recorded")
	}
	usage, _ := e.srv.db.ProjectUsage(ctx, pr.ID)
	e.srv.checkProjectDisk(ctx, pr, usage) // already reported
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(titles) != 2 || titles[0] != "Project A is over its disk limit" {
		t.Fatalf("disk alerts: %q", titles)
	}

	// No limits: room again.
	o.want(http.StatusOK, "PUT", path+"/limits", map[string]any{})
	admA.want(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "b.test", "project_id": e.a})
}
