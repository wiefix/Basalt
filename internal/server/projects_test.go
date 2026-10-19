package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestProjectsAPI: global admins manage projects and their members; new
// resources go to a chosen project or the default one, and move between
// projects; databases link only to apps of their project.
func TestProjectsAPI(t *testing.T) {
	hub := &fakeHub{}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Agents: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: &testClock{}}
	owner := e.setupOwner()

	list := owner.want(http.StatusOK, "GET", "/api/v1/projects", nil).body["projects"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["default"] != true {
		t.Fatalf("projects: %v", list)
	}
	for _, bad := range []map[string]any{{"name": ""}, {"name": "a\nb"}, {"name": "ok", "description": "x\x00"}} {
		owner.want(http.StatusUnprocessableEntity, "POST", "/api/v1/projects", bad)
	}
	shop := owner.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "Shop", "description": "the shop"}).body["project"].(map[string]any)
	sid := strconv.Itoa(int(shop["id"].(float64)))
	owner.want(http.StatusConflict, "POST", "/api/v1/projects", map[string]any{"name": "SHOP"})
	owner.want(http.StatusOK, "PUT", "/api/v1/projects/"+sid, map[string]any{"name": "Web shop", "description": ""})

	// Members.
	pass := "member password 123"
	m := owner.want(http.StatusCreated, "POST", "/api/v1/users", map[string]any{"email": "m@example.com", "name": "M", "password": pass, "role": "member"}).body["user"].(map[string]any)
	mid := strconv.Itoa(int(m["id"].(float64)))
	owner.want(http.StatusUnprocessableEntity, "PUT", "/api/v1/projects/"+sid+"/members/"+mid, map[string]any{"role": "owner"})
	owner.want(http.StatusNotFound, "PUT", "/api/v1/projects/"+sid+"/members/999", map[string]any{"role": "viewer"})
	owner.want(http.StatusNoContent, "PUT", "/api/v1/projects/"+sid+"/members/"+mid, map[string]any{"role": "admin"})
	got := owner.want(http.StatusOK, "GET", "/api/v1/projects/"+sid, nil).body
	if mem := got["members"].([]any); len(mem) != 1 || mem[0].(map[string]any)["role"] != "admin" {
		t.Fatalf("members: %v", mem)
	}
	// A project admin manages its members, but projects stay the panel admins'.
	member := e.client()
	member.login("m@example.com", pass)
	member.want(http.StatusOK, "GET", "/api/v1/projects", nil)
	member.want(http.StatusForbidden, "POST", "/api/v1/projects", map[string]any{"name": "Mine"})
	member.want(http.StatusNoContent, "PUT", "/api/v1/projects/"+sid+"/members/"+mid, map[string]any{"role": "admin"})
	member.want(http.StatusForbidden, "DELETE", "/api/v1/projects/"+sid, nil)

	// Resources: in the chosen project, or the default one.
	z := owner.want(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "example.com", "project_id": shop["id"]}).body["zone"].(map[string]any)
	if z["project_id"] != shop["id"] {
		t.Fatalf("zone: %v", z)
	}
	owner.want(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]any{"name": "example.org", "project_id": 999})
	owner.want(http.StatusCreated, "POST", "/api/v1/health-checks", map[string]any{"name": "shop", "kind": "tcp", "host": "192.0.2.1", "port": 80, "project_id": shop["id"]})
	checks := owner.want(http.StatusOK, "GET", "/api/v1/health-checks", nil).body["checks"].([]any)
	c := checks[0].(map[string]any)
	cid := strconv.Itoa(int(c["id"].(float64)))
	if c["project_id"] != shop["id"] {
		t.Fatalf("check: %v", c)
	}
	owner.want(http.StatusUnprocessableEntity, "PUT", "/api/v1/health-checks/"+cid, map[string]any{"name": "shop", "kind": "tcp", "host": "192.0.2.1", "port": 80, "project_id": 1})

	s := owner.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, s["id"])
	web := owner.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": s["id"], "name": "web", "image": "nginx:1.27", "port": 80, "project_id": shop["id"]}).body["app"].(map[string]any)
	db := owner.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": s["id"], "name": "pg", "source": "database", "engine": "postgres"}).body["app"].(map[string]any)
	if web["project_id"] != shop["id"] || db["project_id"] != float64(1) {
		t.Fatalf("apps: %v / %v", web["project_id"], db["project_id"])
	}
	dbPath := "/api/v1/apps/" + strconv.Itoa(int(db["id"].(float64)))
	owner.want(http.StatusUnprocessableEntity, "POST", dbPath+"/links", map[string]any{"app_id": web["id"]})
	owner.want(http.StatusNoContent, "PUT", dbPath+"/project", map[string]any{"project_id": shop["id"]})
	owner.want(http.StatusOK, "POST", dbPath+"/links", map[string]any{"app_id": web["id"]})
	owner.want(http.StatusConflict, "PUT", dbPath+"/project", map[string]any{"project_id": 1})
	owner.want(http.StatusUnprocessableEntity, "PUT", dbPath+"/project", map[string]any{"project_id": 999})
	owner.want(http.StatusNotFound, "PUT", "/api/v1/sites/999/project", map[string]any{"project_id": 1})
	member.want(http.StatusForbidden, "PUT", "/api/v1/dns/zones/"+strconv.Itoa(int(z["id"].(float64)))+"/project", map[string]any{"project_id": 1})

	// Only empty projects go; the default one never.
	owner.want(http.StatusConflict, "DELETE", "/api/v1/projects/"+sid, nil)
	owner.want(http.StatusConflict, "DELETE", "/api/v1/projects/1", nil)
	owner.want(http.StatusNoContent, "PUT", "/api/v1/dns/zones/"+strconv.Itoa(int(z["id"].(float64)))+"/project", map[string]any{"project_id": 1})
	owner.want(http.StatusNoContent, "PUT", "/api/v1/health-checks/"+cid+"/project", map[string]any{"project_id": 1})
	if c := owner.want(http.StatusOK, "GET", "/api/v1/health-checks", nil).body["checks"].([]any)[0].(map[string]any); c["project_id"] != float64(1) {
		t.Fatalf("moved check: %v", c["project_id"])
	}
	for _, a := range []map[string]any{db, web} {
		owner.want(http.StatusOK, "DELETE", "/api/v1/apps/"+strconv.Itoa(int(a["id"].(float64))), nil)
	}
	owner.want(http.StatusNoContent, "DELETE", "/api/v1/projects/"+sid, nil)
	owner.want(http.StatusNotFound, "GET", "/api/v1/projects/"+sid, nil)
}
