package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestComposeAppReplicas: a compose app without volumes runs on several
// servers; volumes cannot come back while it does (a new file, or a
// rollback to a file that had them).
func TestComposeAppReplicas(t *testing.T) {
	hub := &fakeHub{}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Agents: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := (&env{t: t, srv: srv, ts: ts, clock: &testClock{}}).setupOwner()

	var servers []int64
	for i, name := range []string{"s1", "s2"} {
		s := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": name}).body["server"].(map[string]any)
		id := int64(s["id"].(float64))
		srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0', enrolled_at = 1 WHERE id = ?`, id)
		c.want(http.StatusOK, "PUT", "/api/v1/servers/"+strconv.FormatInt(id, 10)+"/public-ips", map[string]any{"ips": []string{"192.0.2." + strconv.Itoa(10+i)}})
		servers = append(servers, id)
	}
	c.want(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})
	withVolume := "services:\n  web:\n    image: nginx:1.27\n    expose: [\"80\"]\n    volumes: [data:/data]\nvolumes:\n  data:\n"
	stateless := "services:\n  web:\n    image: nginx:1.27\n    expose: [\"80\"]\n  worker:\n    image: busybox:1.37\n    command: [sleep, infinity]\n"
	a := c.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": servers[0], "name": "stack", "source": "compose",
		"compose": withVolume, "service": "web", "port": 80, "domains": []string{"stack.example.test"}, "tls": "internal"}).body["app"].(map[string]any)
	app := "/api/v1/apps/" + strconv.Itoa(int(a["id"].(float64)))
	waitIdle := func() {
		t.Helper()
		for range 200 {
			var n int
			srv.db.R.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status = 'running'`).Scan(&n)
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("deployments still running")
	}
	first := c.want(http.StatusAccepted, "POST", app+"/deploy", map[string]any{}).body["deployment"].(map[string]any)["id"]
	waitIdle()

	replicas := app + "/replicas"
	if r := c.want(http.StatusUnprocessableEntity, "POST", replicas, map[string]any{"server_id": servers[1]}); r.body["error"] == nil {
		t.Fatal("no reason given")
	}
	c.want(http.StatusOK, "PUT", app+"/compose", map[string]any{"compose": stateless, "service": "web", "port": 80})
	r := c.want(http.StatusCreated, "POST", replicas, map[string]any{"server_id": servers[1]}).body
	if r["deploying"] != true {
		t.Fatalf("add replica = %v", r)
	}
	waitIdle()
	// The deployment ran the whole file on both servers, in turn.
	var on []int64
	for i, spec := range hub.deployed() {
		if len(spec.Services) == 2 {
			on = append(on, hub.servers[i])
		}
	}
	if len(on) != 2 || on[0] != servers[0] || on[1] != servers[1] {
		t.Fatalf("deployed on %v, want %v", on, servers)
	}
	// Volumes stay out while it runs on several servers.
	c.want(http.StatusUnprocessableEntity, "PUT", app+"/compose", map[string]any{"compose": withVolume, "service": "web", "port": 80})
	c.want(http.StatusUnprocessableEntity, "POST", app+"/deploy", map[string]any{"rollback_to": first})
	// Without the replica, the rollback is allowed again.
	c.want(http.StatusOK, "DELETE", replicas+"/"+strconv.FormatInt(servers[1], 10), nil)
	c.want(http.StatusAccepted, "POST", app+"/deploy", map[string]any{"rollback_to": first})
	waitIdle()
}
