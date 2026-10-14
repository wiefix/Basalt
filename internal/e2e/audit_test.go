package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/server"
)

// TestAuditLog: admins page through and filter the audit log; a change
// made in the database outside the panel breaks its chain and is reported.
func TestAuditLog(t *testing.T) {
	got := make(chan map[string]any, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		got <- m
	}))
	t.Cleanup(receiver.Close)

	p := startPanelWith(t, server.Options{AuditCheckEvery: 200 * time.Millisecond})
	p.call(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "ops", "kind": "webhook",
		"url": receiver.URL, "allow_private": true, "events": []string{"audit.broken"}})
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	for i := range 3 {
		p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": fmt.Sprintf("node-%d", i)})
	}

	list := func(query string) map[string]any {
		t.Helper()
		return p.call(200, "GET", "/api/v1/audit"+query, nil)
	}
	entries := func(m map[string]any) []map[string]any {
		var out []map[string]any
		for _, e := range m["entries"].([]any) {
			out = append(out, e.(map[string]any))
		}
		return out
	}
	all := entries(list(""))
	if len(all) < 6 || all[0]["action"] != "server.created" || all[0]["actor"].(map[string]any)["email"] != "o@example.com" ||
		all[0]["details"].(map[string]any)["name"] != "node-2" || all[0]["ip"] == "" {
		t.Fatalf("entries: %v", all)
	}
	if acts := fmt.Sprint(list("")["actions"]); !strings.Contains(acts, "server.created") || !strings.Contains(acts, "user.created") {
		t.Fatalf("actions: %s", acts)
	}
	// Filters and pages.
	servers := entries(list("?action=server.&limit=2"))
	if len(servers) != 2 || servers[0]["details"].(map[string]any)["name"] != "node-2" || list("?action=server.&limit=2")["more"] != true {
		t.Fatalf("by action: %v", servers)
	}
	rest := entries(list(fmt.Sprintf("?action=server.&limit=2&before=%v", servers[1]["id"])))
	if len(rest) != 1 || rest[0]["details"].(map[string]any)["name"] != "node-0" {
		t.Fatalf("next page: %v", rest)
	}
	owner := all[0]["actor"].(map[string]any)["id"]
	if mine := entries(list(fmt.Sprintf("?actor=%v", owner))); len(mine) != len(all) {
		t.Fatalf("by actor: %d of %d", len(mine), len(all))
	}
	if sys := list("?actor=system")["entries"].([]any); len(sys) != 0 {
		t.Fatalf("system entries: %v", sys)
	}
	for _, bad := range []string{"?before=x", "?limit=0", "?limit=500", "?actor=bob", "?action=" + strings.Repeat("a", 101)} {
		p.call(http.StatusUnprocessableEntity, "GET", "/api/v1/audit"+bad, nil)
	}
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(http.StatusForbidden, "GET", "/api/v1/audit", nil)
	viewer.call(http.StatusForbidden, "GET", "/api/v1/audit/verify", nil)

	// The chain holds, with every entry written by this panel.
	check := p.call(200, "GET", "/api/v1/audit/verify", nil)
	if check["intact"] != true || check["legacy"] != float64(0) || check["entries"] != float64(len(entries(list("")))) {
		t.Fatalf("verify: %v", check)
	}
	// The log cannot be changed through SQL as it is...
	if _, err := p.db.W.Exec(`UPDATE audit_log SET ip = '' WHERE action = 'server.created'`); err == nil {
		t.Fatal("audit log updated")
	}
	// ...and a change that gets around it is found, by the API and the
	// periodic check, which notifies once.
	p.db.W.Exec(`DROP TRIGGER audit_log_no_update`)
	var target int64
	p.db.R.QueryRow(`SELECT id FROM audit_log WHERE action = 'user.created'`).Scan(&target)
	if _, err := p.db.W.Exec(`UPDATE audit_log SET details = '{"email":"someone@example.com"}' WHERE id = ?`, target); err != nil {
		t.Fatal(err)
	}
	check = p.call(200, "GET", "/api/v1/audit/verify", nil)
	if check["intact"] != false || check["broken_at"] != float64(target) {
		t.Fatalf("verify after tampering: %v", check)
	}
	watch, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go p.srv.WatchAuditLog(watch)
	select {
	case m := <-got:
		if m["event"] != "audit.broken" || m["title"] != fmt.Sprintf("The audit log was changed at entry #%d", target) {
			t.Fatalf("message: %v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}
	select {
	case m := <-got:
		t.Fatalf("notified again: %v", m)
	case <-time.After(time.Second):
	}
	// New entries keep being written and listed.
	p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "node-3"})
	if e := entries(list("?limit=1")); e[0]["details"].(map[string]any)["name"] != "node-3" {
		t.Fatalf("after tampering: %v", e)
	}
}
