package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// fakeHub is an agent channel to one always-connected server that accepts
// every call and records the apps it is asked to deploy.
type fakeHub struct {
	mu      sync.Mutex
	deploys []agentproto.AppSpec
	servers []int64 // the server of each deployment
}

func (h *fakeHub) Connected(int64) bool { return true }
func (h *fakeHub) Call(context.Context, int64, string, any, any) error {
	return nil
}
func (h *fakeHub) Stream(_ context.Context, serverID int64, msgType string, req, resp any, _ func(json.RawMessage) error) error {
	if spec, ok := req.(agentproto.AppSpec); ok && msgType == agentproto.TypeAppDeploy {
		h.mu.Lock()
		h.deploys = append(h.deploys, spec)
		h.servers = append(h.servers, serverID)
		h.mu.Unlock()
		if r, ok := resp.(*agentproto.AppDeployResponse); ok {
			r.OK = true
		}
	}
	return nil
}
func (h *fakeHub) Disconnect(int64)      {}
func (h *fakeHub) CAFingerprint() string { return "" }

func (h *fakeHub) deployed() []agentproto.AppSpec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]agentproto.AppSpec(nil), h.deploys...)
}

// TestDatabasePasswordRedeploysWithNewURL: after a database's new password,
// a linked app is redeployed with the new connection URL, not the old one.
func TestDatabasePasswordRedeploysWithNewURL(t *testing.T) {
	hub := &fakeHub{}
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	c := e.setupOwner()

	s := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	serverID := int64(s["id"].(float64))
	if _, err := srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, serverID); err != nil {
		t.Fatal(err)
	}
	create := func(b map[string]any) string {
		b["server_id"] = serverID
		a := c.want(http.StatusCreated, "POST", "/api/v1/apps", b).body["app"].(map[string]any)
		return strconv.Itoa(int(a["id"].(float64)))
	}
	web := create(map[string]any{"name": "web", "image": "nginx:1.27", "port": 80})
	cache := create(map[string]any{"name": "cache", "source": "database", "engine": "redis"})
	c.want(http.StatusOK, "POST", "/api/v1/apps/"+cache+"/links", map[string]any{"app_id": mustAtoi(t, web), "env_var": "REDIS_URL"})
	oldURL := c.want(http.StatusOK, "GET", "/api/v1/apps/"+web+"/env", nil).body["env"].(map[string]any)["REDIS_URL"]

	// web has been deployed once (the fake agent fails it; it counts).
	c.want(http.StatusAccepted, "POST", "/api/v1/apps/"+web+"/deploy", map[string]any{})
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
	waitIdle()

	r := c.want(http.StatusOK, "POST", "/api/v1/apps/"+cache+"/password", map[string]any{}).body
	newURL := r["connection"].(map[string]any)["url"]
	if newURL == oldURL || len(r["redeploying"].([]any)) != 1 {
		t.Fatalf("password change = %v", r)
	}
	waitIdle()
	var last *agentproto.AppSpec
	for _, spec := range hub.deployed() {
		if strconv.FormatInt(spec.AppID, 10) == web {
			last = &spec
		}
	}
	if last == nil || last.Env["REDIS_URL"] != newURL {
		t.Fatalf("web redeployed with REDIS_URL %v, want %v", last, newURL)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestScheduledBackupThatCannotStart: a scheduled backup that cannot start
// (an app without volumes) is recorded as failed once, not retried every
// 30 seconds.
func TestScheduledBackupThatCannotStart(t *testing.T) {
	hub := &fakeHub{}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Agents: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: &testClock{}}
	c := e.setupOwner()

	s := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	serverID := int64(s["id"].(float64))
	srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0', restic = '0.18.0' WHERE id = ?`, serverID)
	app := c.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "web", "image": "nginx:1.27", "port": 80}).body["app"].(map[string]any)
	id := strconv.Itoa(int(app["id"].(float64)))
	st := c.want(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{"name": "local", "kind": "local", "path": "/var/backups/basalt"}).body["storage"].(map[string]any)
	c.want(http.StatusOK, "PUT", "/api/v1/apps/"+id+"/backup-policy", map[string]any{
		"storage_id": st["id"], "schedule": "hourly", "at_minute": 0, "keep_last": 1})
	// The policy was set before the last slot.
	srv.db.W.Exec(`UPDATE backup_policies SET updated_at = updated_at - 7200`)

	for range 3 {
		srv.runDueBackups(context.Background())
	}
	runs := c.want(http.StatusOK, "GET", "/api/v1/apps/"+id+"/backups", nil).body["backups"].([]any)
	if len(runs) != 1 {
		t.Fatalf("%d runs recorded: %v", len(runs), runs)
	}
	run := runs[0].(map[string]any)
	if run["status"] != "failed" || run["trigger"] != "schedule" || run["error"] != "this app keeps no data: it has no volumes to back up" {
		t.Fatalf("run = %v", run)
	}
}
