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

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

// TestProjectChannels: a project's admins manage its notification channels,
// which get only the events about its resources; the panel's channels get
// every event; nobody else sees or changes them.
func TestProjectChannels(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{} // receiver ("webhook", "telegram") -> titles
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct{ Title, Text string }
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(r.URL.Path, "/sendMessage") {
			got["telegram"] = append(got["telegram"], m.Text)
		} else {
			got["webhook"] = append(got["webhook"], m.Title)
		}
	}))
	t.Cleanup(receiver.Close)
	received := func(who string) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got[who]...)
	}

	// Telegram's API is the receiver (its deliveries may then reach it).
	e := newProjectEnvWith(t, Options{TelegramAPI: receiver.URL})
	o := e.owner
	b := o.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "B"}).body["project"].(map[string]any)["id"].(float64)
	admA := e.user("adm-a@example.com", store.RoleUser, map[float64]store.Role{e.a: store.RoleAdmin})
	admB := e.user("adm-b@example.com", store.RoleUser, map[float64]store.Role{b: store.RoleAdmin})
	memA := e.user("mem-a@example.com", store.RoleUser, map[float64]store.Role{e.a: store.RoleMember})
	viewer := e.user("viewer@example.com", store.RoleViewer, nil)

	panel := o.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "ops", "kind": "webhook",
		"url": receiver.URL, "allow_private": true, "events": []string{EventHealthDown, EventServerDown}}).body["channel"].(map[string]any)
	if panel["project_id"] != float64(0) {
		t.Fatalf("panel channel: %v", panel)
	}

	// Only those who manage a channel see the list.
	memA.want(http.StatusForbidden, "GET", "/api/v1/notifications", nil)
	viewer.want(http.StatusForbidden, "GET", "/api/v1/notifications", nil)
	if l := admA.want(http.StatusOK, "GET", "/api/v1/notifications", nil).body; len(l["channels"].([]any)) != 0 || l["panel_url"] != "" {
		t.Fatalf("project admin's list: %v", l)
	}

	tg := func(name string, project float64, events ...string) map[string]any {
		return map[string]any{"name": name, "kind": "telegram", "token": "123:abc", "chat_id": "42", "project_id": project, "events": events}
	}
	// What a project admin may not do.
	admA.want(http.StatusForbidden, "POST", "/api/v1/notifications", tg("x", 0, EventHealthDown))
	admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/notifications", tg("x", b, EventHealthDown))
	memA.want(http.StatusForbidden, "POST", "/api/v1/notifications", tg("x", e.a, EventHealthDown))
	admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/notifications", tg("x", e.a, EventServerDown))
	admA.want(http.StatusUnprocessableEntity, "POST", "/api/v1/notifications", map[string]any{"name": "x", "kind": "webhook",
		"url": "http://10.0.0.1/hook", "allow_private": true, "project_id": e.a, "events": []string{EventHealthDown}})

	// A project's channel; names are unique within a project.
	ch := admA.want(http.StatusCreated, "POST", "/api/v1/notifications", tg("ops", e.a, EventHealthDown, EventDeployFailed)).body["channel"].(map[string]any)
	if ch["project_id"] != e.a {
		t.Fatalf("project channel: %v", ch)
	}
	admA.want(http.StatusConflict, "POST", "/api/v1/notifications", tg("ops", e.a, EventHealthDown))
	path := "/api/v1/notifications/" + num(ch["id"].(float64))
	panelPath := "/api/v1/notifications/" + num(panel["id"].(float64))
	if l := admA.want(http.StatusOK, "GET", "/api/v1/notifications", nil).body["channels"].([]any); len(l) != 1 {
		t.Fatalf("project admin sees %d channels", len(l))
	}
	if l := o.want(http.StatusOK, "GET", "/api/v1/notifications", nil).body["channels"].([]any); len(l) != 2 {
		t.Fatalf("owner sees %d channels", len(l))
	}

	// Others' channels are not found; the project stays.
	admB.want(http.StatusNotFound, "PUT", path, tg("ops", 0, EventHealthDown))
	admB.want(http.StatusNotFound, "DELETE", path, nil)
	admA.want(http.StatusNotFound, "DELETE", panelPath, nil)
	admA.want(http.StatusNotFound, "POST", panelPath+"/test", nil)
	memA.want(http.StatusForbidden, "DELETE", path, nil)
	viewer.want(http.StatusForbidden, "DELETE", panelPath, nil)
	admA.want(http.StatusUnprocessableEntity, "PUT", path, tg("ops", b, EventHealthDown))
	admA.want(http.StatusUnprocessableEntity, "PUT", path, tg("ops", 0, EventAuditBroken))
	admA.want(http.StatusOK, "PUT", path, map[string]any{"name": "ops", "chat_id": "42", "events": []string{EventHealthDown}})
	if r := admA.want(http.StatusOK, "POST", path+"/test", nil).body; r["ok"] != true {
		t.Fatalf("test message: %v", r)
	}

	// A health check of project A goes down: both channels hear of it; an
	// event about project B reaches only the panel's channel.
	ctx := context.Background()
	if err := e.srv.loadHealth(ctx); err != nil {
		t.Fatal(err)
	}
	e.srv.HealthReport(int64(e.serverID), agentproto.HealthReport{Results: []agentproto.HealthResult{{CheckID: int64(e.check), Error: "refused"}}})
	e.srv.notify(notify.Message{Event: EventHealthDown, Title: "b is down", Project: int64(b)})
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %v", what, got)
			}
		}
	}
	waitFor("deliveries", func() bool { return len(received("webhook")) == 2 && len(received("telegram")) == 2 })
	time.Sleep(100 * time.Millisecond)
	if tgm := received("telegram"); len(tgm) != 2 || !strings.HasPrefix(tgm[1], "a is down") {
		t.Fatalf("project channel got %q", tgm)
	}

	// The audit entries about the project's channel are the project's.
	acts := admA.want(http.StatusOK, "GET", "/api/v1/projects/"+num(e.a)+"/audit", nil).body["actions"].([]any)
	if !strings.Contains(strings.Join(func() []string {
		var s []string
		for _, a := range acts {
			s = append(s, a.(string))
		}
		return s
	}(), " "), "notification.created") {
		t.Fatalf("project audit actions: %v", acts)
	}
	admA.want(http.StatusNoContent, "DELETE", path, nil)
}

// TestProjectChannelPublicOnly: a project's email channel does not reach a
// mail server on a private address.
func TestProjectChannelPublicOnly(t *testing.T) {
	e := newProjectEnv(t)
	c := e.owner.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "mail", "kind": "email",
		"smtp_host": "127.0.0.1", "smtp_port": 25, "smtp_tls": "none", "from": "basalt@example.com", "to": []string{"ops@example.com"},
		"project_id": e.a, "events": []string{EventDeployFailed}}).body["channel"].(map[string]any)
	r := e.owner.want(http.StatusOK, "POST", "/api/v1/notifications/"+num(c["id"].(float64))+"/test", nil).body
	if r["ok"] != false || !strings.Contains(r["error"].(string), "not a public address") {
		t.Fatalf("test of a private mail server: %v", r)
	}
}
