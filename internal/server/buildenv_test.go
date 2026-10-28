package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestBuildEnv: a git app names the variables its build gets; the build
// gets exactly those of its environment (not the others); names the build
// tools read are refused.
func TestBuildEnv(t *testing.T) {
	hub := &fakeHub{}
	clock := &testClock{t: time.Now()}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	c := e.setupOwner()
	s := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, s["id"])
	app := func(env []string, code int) map[string]any {
		return c.want(code, "POST", "/api/v1/apps", map[string]any{"server_id": s["id"], "name": "web", "source": "git", "port": 80,
			"git": map[string]any{"url": "https://git.example.com/o/web.git", "build_env": env}}).body
	}
	for _, bad := range [][]string{{"PATH"}, {"DOCKER_HOST"}, {"buildkit_x"}, {"GIT_SSH_COMMAND"}, {"1A"}, {"A-B"}} {
		app(bad, http.StatusUnprocessableEntity)
	}
	a := app([]string{"NEXT_PUBLIC_API", "SENTRY_TOKEN", "NEXT_PUBLIC_API", "MISSING"}, http.StatusCreated)["app"].(map[string]any)
	id := strconv.Itoa(int(a["id"].(float64)))
	if be := a["git"].(map[string]any)["build_env"].([]any); len(be) != 3 {
		t.Fatalf("build_env: %v", be)
	}
	c.want(http.StatusOK, "PUT", "/api/v1/apps/"+id+"/env", map[string]any{"env": map[string]string{
		"NEXT_PUBLIC_API": "https://api.example.com", "SENTRY_TOKEN": "s3cret", "DATABASE_URL": "postgres://x"}})
	c.want(http.StatusAccepted, "POST", "/api/v1/apps/"+id+"/deploy", map[string]any{})
	for range 300 {
		if len(hub.deployed()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	d := hub.deployed()
	if len(d) != 1 || len(d[0].Git.BuildEnv) != 2 || d[0].Git.BuildEnv["SENTRY_TOKEN"] != "s3cret" ||
		d[0].Git.BuildEnv["NEXT_PUBLIC_API"] != "https://api.example.com" || d[0].Git.BuildEnv["DATABASE_URL"] != "" {
		t.Fatalf("build env: %+v", d)
	}
}
