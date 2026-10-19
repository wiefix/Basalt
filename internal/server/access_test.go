package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/store"
)

// projectEnv is a panel with a fake agent, an owner, and project "A"
// holding a site, an app, a database, a DNS zone and a health check.
type projectEnv struct {
	*env
	owner                      *client
	a                          float64 // project A
	site, app, db, zone, check float64
	serverID                   float64
}

func newProjectEnv(t *testing.T) *projectEnv {
	t.Helper()
	return newProjectEnvWith(t, Options{})
}

// newProjectEnvWith is newProjectEnv with more options.
func newProjectEnvWith(t *testing.T, opts Options) *projectEnv {
	t.Helper()
	opts.SetupToken, opts.Agents = testSetupToken, &fakeHub{}
	srv := newTestServerWith(t, opts)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &projectEnv{env: &env{t: t, srv: srv, ts: ts, clock: &testClock{}}}
	o := e.setupOwner()
	e.owner = o
	e.a = o.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "A"}).body["project"].(map[string]any)["id"].(float64)
	s := o.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	e.serverID = s["id"].(float64)
	srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0', enrolled_at = 1 WHERE id = ?`, e.serverID)
	id := func(r resp, key string) float64 { return r.body[key].(map[string]any)["id"].(float64) }
	e.zone = id(o.want(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "a.test", "project_id": e.a}), "zone")
	e.site = id(o.want(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{"server_id": e.serverID, "name": "blog",
		"domains": []string{"blog.a.test"}, "tls": "internal", "project_id": e.a}), "site")
	e.app = id(o.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID, "name": "web",
		"image": "nginx:1.27", "port": 80, "domains": []string{"web.a.test"}, "tls": "internal", "project_id": e.a}), "app")
	e.db = id(o.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID, "name": "pg",
		"source": "database", "engine": "postgres", "project_id": e.a}), "app")
	o.want(http.StatusCreated, "POST", "/api/v1/health-checks", map[string]any{"name": "a", "kind": "tcp", "host": "192.0.2.1", "port": 80, "project_id": e.a})
	e.check = o.want(http.StatusOK, "GET", "/api/v1/health-checks", nil).body["checks"].([]any)[0].(map[string]any)["id"].(float64)
	return e
}

// user creates a user with a global role and the given project roles.
func (e *projectEnv) user(email string, global store.Role, roles map[float64]store.Role) *client {
	e.t.Helper()
	pass := "user password 1234"
	u := e.owner.want(http.StatusCreated, "POST", "/api/v1/users", map[string]any{"email": email, "name": email, "password": pass, "role": global}).body["user"].(map[string]any)
	for p, r := range roles {
		e.owner.want(http.StatusNoContent, "PUT", "/api/v1/projects/"+num(p)+"/members/"+num(u["id"].(float64)), map[string]any{"role": r})
	}
	c := e.client()
	if r := c.login(email, pass); r.status != http.StatusOK {
		e.t.Fatalf("login %s: %d", email, r.status)
	}
	return c
}

func num(f float64) string { return strconv.Itoa(int(f)) }

var pathParamRE = regexp.MustCompile(`\{\w+\}`)

// TestEveryProjectRoute: whoever has no role in a resource's project gets
// 404 from every route about it, whatever it does; a user without a role
// on the panel gets 403 from every route of the panel itself. A route that
// forgets its check fails here.
func TestEveryProjectRoute(t *testing.T) {
	e := newProjectEnv(t)
	b := e.owner.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "B"}).body["project"].(map[string]any)["id"].(float64)
	outsider := e.user("out@example.com", store.RoleUser, map[float64]store.Role{b: store.RoleAdmin})

	project, global := 0, 0
	for _, rt := range e.srv.routes {
		if rt.access == nil || rt.access.mfaPending || !strings.HasPrefix(rt.path, "/api/") {
			continue
		}
		path := rt.path
		if rt.access.project {
			project++
			var id float64
			switch {
			case strings.HasPrefix(path, "/api/v1/sites/{id}"):
				id = e.site
			case strings.HasPrefix(path, "/api/v1/apps/{id}/connection"), strings.HasPrefix(path, "/api/v1/apps/{id}/links"),
				strings.HasPrefix(path, "/api/v1/apps/{id}/password"):
				id = e.db
			case strings.HasPrefix(path, "/api/v1/apps/{id}"):
				id = e.app
			case strings.HasPrefix(path, "/api/v1/dns/zones/{id}"):
				id = e.zone
			case strings.HasPrefix(path, "/api/v1/health-checks/{id}"):
				id = e.check
			default:
				t.Errorf("%s %s: a project route about an unknown resource", rt.method, path)
				continue
			}
			path = strings.Replace(path, "{id}", num(id), 1)
		} else if rt.access.role == store.RoleUser || rt.access.role == "" {
			continue // anyone signed in, or public
		} else {
			global++
		}
		path = pathParamRE.ReplaceAllString(path, "1")
		want := http.StatusNotFound
		if !rt.access.project {
			want = http.StatusForbidden
		}
		if r := outsider.do(rt.method, path, map[string]any{}); r.status != want {
			t.Errorf("%s %s as an outsider: %d %v; want %d", rt.method, path, r.status, r.body["error"], want)
		}
	}
	if project < 50 || global < 50 {
		t.Fatalf("checked %d project and %d panel routes: the route table changed?", project, global)
	}
}

// TestProjectRoles: what a project role allows, lists that show only what
// one may see, and names that stay with the project of their DNS zone.
func TestProjectRoles(t *testing.T) {
	e := newProjectEnv(t)
	b := e.owner.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "B"}).body["project"].(map[string]any)["id"].(float64)
	viewer := e.user("view@example.com", store.RoleUser, map[float64]store.Role{e.a: store.RoleViewer})
	admin := e.user("adm@example.com", store.RoleUser, map[float64]store.Role{b: store.RoleAdmin, e.a: store.RoleMember})

	// Lists show the projects one belongs to.
	if l := viewer.want(http.StatusOK, "GET", "/api/v1/projects", nil).body["projects"].([]any); len(l) != 1 {
		t.Fatalf("viewer's projects: %v", l)
	}
	if l := viewer.want(http.StatusOK, "GET", "/api/v1/apps", nil).body["apps"].([]any); len(l) != 2 {
		t.Fatalf("viewer's apps: %d", len(l))
	}
	other := e.user("none@example.com", store.RoleUser, nil)
	for _, path := range []string{"/api/v1/sites", "/api/v1/apps"} {
		key := strings.TrimPrefix(path, "/api/v1/")
		if l := other.want(http.StatusOK, "GET", path, nil).body[key].([]any); len(l) != 0 {
			t.Fatalf("%s of a user in no project: %v", path, l)
		}
	}
	if d := other.want(http.StatusOK, "GET", "/api/v1/dns", nil).body; len(d["zones"].([]any)) != 0 || len(d["servers"].([]any)) != 0 {
		t.Fatalf("dns of a user in no project: %v", d)
	}
	// Servers: only what placing a resource needs.
	sv := other.want(http.StatusOK, "GET", "/api/v1/servers", nil).body["servers"].([]any)[0].(map[string]any)
	if sv["docker"] != "28.0.0" || sv["last_ip"] != nil || sv["hostname"] != nil {
		t.Fatalf("server for a user: %v", sv)
	}
	other.want(http.StatusForbidden, "GET", "/api/v1/servers/"+num(e.serverID), nil)
	other.want(http.StatusForbidden, "GET", "/api/v1/users", nil)

	// A viewer reads, a member deploys, only an admin changes settings.
	site := "/api/v1/sites/" + num(e.site)
	viewer.want(http.StatusOK, "GET", site, nil)
	viewer.want(http.StatusForbidden, "POST", site+"/files/mkdir", map[string]any{"path": "/x"})
	viewer.want(http.StatusForbidden, "POST", "/api/v1/sites", map[string]any{"server_id": e.serverID, "name": "x", "domains": []string{"x.a.test"}, "project_id": e.a})
	app := "/api/v1/apps/" + num(e.app)
	admin.want(http.StatusAccepted, "POST", app+"/deploy", map[string]any{})
	admin.want(http.StatusForbidden, "PUT", app+"/env", map[string]any{"env": map[string]string{}})
	admin.want(http.StatusForbidden, "DELETE", site, nil)

	// Creating: in a project one administers; another one is not found.
	admin.want(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]any{"name": "x.test", "project_id": 999})
	other.want(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]any{"name": "x.test", "project_id": e.a})
	admin.want(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones", map[string]any{"name": "x.test"})          // the default project
	admin.want(http.StatusForbidden, "POST", "/api/v1/dns/zones", map[string]any{"name": "x.test", "project_id": e.a}) // a member there
	zb := admin.want(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "b.test", "project_id": b}).body["zone"].(map[string]any)
	// A zone over the names of another project's sites or apps is refused,
	// and so is a name in another project's zone.
	admin.want(http.StatusConflict, "POST", "/api/v1/dns/zones", map[string]any{"name": "blog.a.test", "project_id": b})
	app2 := admin.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID, "name": "web2",
		"image": "nginx:1.27", "port": 80, "domains": []string{"web2.b.test"}, "tls": "internal", "project_id": b}).body["app"].(map[string]any)
	admin.want(http.StatusConflict, "PUT", "/api/v1/apps/"+num(app2["id"].(float64))+"/domains", map[string]any{"domains": []string{"shop.a.test"}})
	e.owner.want(http.StatusConflict, "PUT", "/api/v1/apps/"+num(e.app)+"/project", map[string]any{"project_id": b})
	e.owner.want(http.StatusConflict, "PUT", "/api/v1/dns/zones/"+num(e.zone)+"/project", map[string]any{"project_id": b})
	// A record follows only a check of its zone's project.
	admin.want(http.StatusUnprocessableEntity, "POST", "/api/v1/dns/zones/"+num(zb["id"].(float64))+"/records",
		map[string]any{"name": "www", "type": "A", "value": "192.0.2.9", "ttl": 60, "health_check": e.check})
	// A database links only to apps of its project.
	e.owner.want(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+num(e.db)+"/links", map[string]any{"app_id": app2["id"]})

	// Project admins manage members; projects themselves are the panel's.
	admin.want(http.StatusNoContent, "PUT", "/api/v1/projects/"+num(b)+"/members/2", map[string]any{"role": "viewer"})
	admin.want(http.StatusForbidden, "PUT", "/api/v1/projects/"+num(e.a)+"/members/2", map[string]any{"role": "admin"})
	admin.want(http.StatusForbidden, "DELETE", "/api/v1/projects/"+num(b), nil)

	// A token of a project admin acts at most with its own role.
	secret := admin.want(http.StatusCreated, "POST", "/api/v1/account/tokens", map[string]any{"name": "ci", "role": "member", "expires_days": 0,
		"password": "user password 1234"}).body["secret"].(string)
	admin.want(http.StatusUnprocessableEntity, "POST", "/api/v1/account/tokens", map[string]any{"name": "x", "role": "owner", "expires_days": 0,
		"password": "user password 1234"})
	req, _ := http.NewRequest("DELETE", e.ts.URL+"/api/v1/apps/"+num(app2["id"].(float64)), nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("member token deletes an app: %v %v", r.StatusCode, err)
	}
}

// bearer calls the API with an API token.
func (e *projectEnv) bearer(secret, method, path string) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	r.Body.Close()
	return r.StatusCode
}

// TestProjectTokens: a token limited to a project reaches only that
// project's resources, with at most the role given and the user's there.
func TestProjectTokens(t *testing.T) {
	e := newProjectEnv(t)
	b := e.owner.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "B"}).body["project"].(map[string]any)["id"].(float64)
	tok := func(c *client, role string, project float64) resp {
		return c.do("POST", "/api/v1/account/tokens", map[string]any{"name": "t" + role + num(project), "role": role, "expires_days": 0,
			"project_id": project, "password": ownerPass})
	}
	// The owner's token for project A, as a member.
	r := tok(e.owner, "member", e.a)
	if r.status != http.StatusCreated || r.body["token"].(map[string]any)["project_id"] != e.a {
		t.Fatalf("project token: %d %v", r.status, r.body)
	}
	secret := r.body["secret"].(string)
	site := "/api/v1/sites/" + num(e.site)
	if s := e.bearer(secret, "GET", site); s != http.StatusOK {
		t.Fatalf("token reads its project's site: %d", s)
	}
	if s := e.bearer(secret, "DELETE", site); s != http.StatusForbidden {
		t.Fatalf("member token deletes a site: %d", s)
	}
	// Nothing outside its project: not the default project, not the panel.
	other := e.owner.want(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{"server_id": e.serverID, "name": "main",
		"domains": []string{"main.example.test"}, "tls": "internal"}).body["site"].(map[string]any)
	if s := e.bearer(secret, "GET", "/api/v1/sites/"+num(other["id"].(float64))); s != http.StatusNotFound {
		t.Fatalf("token reads another project's site: %d", s)
	}
	for _, path := range []string{"/api/v1/servers/" + num(e.serverID), "/api/v1/users", "/api/v1/audit"} {
		if s := e.bearer(secret, "GET", path); s != http.StatusForbidden {
			t.Fatalf("project token GET %s: %d", path, s)
		}
	}
	// A project admin cannot give a token more than their role there, nor
	// a token for a project they are not in.
	pass := "user password 1234"
	u := e.owner.want(http.StatusCreated, "POST", "/api/v1/users", map[string]any{"email": "m@example.com", "name": "M", "password": pass, "role": "user"}).body["user"].(map[string]any)
	e.owner.want(http.StatusNoContent, "PUT", "/api/v1/projects/"+num(e.a)+"/members/"+num(u["id"].(float64)), map[string]any{"role": "member"})
	m := e.client()
	m.login("m@example.com", pass)
	body := func(role string, project float64) map[string]any {
		return map[string]any{"name": "x" + role + num(project), "role": role, "expires_days": 0, "project_id": project, "password": pass}
	}
	m.want(http.StatusUnprocessableEntity, "POST", "/api/v1/account/tokens", body("admin", e.a))
	m.want(http.StatusUnprocessableEntity, "POST", "/api/v1/account/tokens", body("viewer", b))
	m.want(http.StatusCreated, "POST", "/api/v1/account/tokens", body("viewer", e.a))
	// A deleted project takes its tokens along.
	if s := e.bearer(tok(e.owner, "viewer", b).body["secret"].(string), "GET", "/api/v1/projects"); s != http.StatusOK {
		t.Fatalf("project B token: %d", s)
	}
	bt := tok(e.owner, "admin", b).body["secret"].(string)
	e.owner.want(http.StatusNoContent, "DELETE", "/api/v1/projects/"+num(b), nil)
	if s := e.bearer(bt, "GET", "/api/v1/projects"); s != http.StatusUnauthorized {
		t.Fatalf("token of a deleted project: %d", s)
	}
}

// TestProjectAudit: entries about a project's resources carry it, and its
// admins read them; nobody else does.
func TestProjectAudit(t *testing.T) {
	e := newProjectEnv(t)
	adm := e.user("adm@example.com", store.RoleUser, map[float64]store.Role{e.a: store.RoleAdmin})
	view := e.user("v@example.com", store.RoleUser, map[float64]store.Role{e.a: store.RoleViewer})
	out := e.user("o@example.com", store.RoleUser, nil)
	adm.want(http.StatusNotFound, "DELETE", "/api/v1/dns/zones/"+num(e.zone)+"/records/999", nil) // fails: not audited
	adm.want(http.StatusOK, "PUT", "/api/v1/sites/"+num(e.site)+"/domains", map[string]any{"domains": []string{"blog2.a.test"}})
	adm.want(http.StatusAccepted, "POST", "/api/v1/apps/"+num(e.app)+"/deploy", map[string]any{})
	e.owner.want(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "default.test"})

	path := "/api/v1/projects/" + num(e.a) + "/audit"
	got := adm.want(http.StatusOK, "GET", path, nil).body
	var actions []string
	for _, x := range got["entries"].([]any) {
		en := x.(map[string]any)
		if en["details"].(map[string]any)["project_id"] != e.a {
			t.Fatalf("entry of another project: %v", en)
		}
		actions = append(actions, en["action"].(string))
	}
	joined := strings.Join(actions, " ")
	for _, want := range []string{"site.domains_changed", "app.deploy_started", "site.created", "app.created", "dns_zone.created", "health.created"} {
		if !strings.Contains(joined, want) {
			t.Errorf("project audit lacks %s: %s", want, joined)
		}
	}
	if strings.Contains(joined, "default.test") || len(got["actions"].([]any)) == 0 {
		t.Fatalf("actions: %v", got["actions"])
	}
	view.want(http.StatusForbidden, "GET", path, nil)
	out.want(http.StatusNotFound, "GET", path, nil)
}
