package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/server"
)

func TestNotifications(t *testing.T) {
	old := agentproto.MinHealthInterval
	agentproto.MinHealthInterval = 1
	t.Cleanup(func() { agentproto.MinHealthInterval = old })

	type delivery struct {
		path, sig, event string
		body             []byte
	}
	got := make(chan delivery, 32)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- delivery{r.URL.Path, r.Header.Get("X-Basalt-Signature"), r.Header.Get("X-Basalt-Event"), b}
	}))
	t.Cleanup(receiver.Close)
	next := func(what string) delivery {
		t.Helper()
		select {
		case d := <-got:
			return d
		case <-time.After(20 * time.Second):
			t.Fatalf("no delivery: %s", what)
			return delivery{}
		}
	}

	p := startPanelWith(t, server.Options{TelegramAPI: receiver.URL, NotifyRetries: []time.Duration{0, 50 * time.Millisecond}})
	h := startHost(t, p)
	create := func(want int, body map[string]any) map[string]any {
		return p.call(want, "POST", "/api/v1/notifications", body)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "kind": "sms"},
		{"name": "bad name!", "kind": "webhook", "url": "https://example.com/h"},
		{"name": "x", "kind": "webhook", "url": "https://example.com/h", "events": []string{"nope"}},
		{"name": "x", "kind": "webhook", "url": "http://example.com/h"},
		{"name": "x", "kind": "webhook", "url": "https://user:pw@example.com/h"},
		{"name": "x", "kind": "slack", "url": "https://example.com/services/x"},
		{"name": "x", "kind": "discord", "url": "https://discord.example.com/api/webhooks/1"},
		{"name": "x", "kind": "telegram", "token": "nocolon", "chat_id": "1"},
		{"name": "x", "kind": "telegram", "token": "1:A", "chat_id": "chat"},
		{"name": "x", "kind": "email", "smtp_host": "smtp.example.com", "smtp_port": 25, "smtp_tls": "none",
			"smtp_user": "u", "smtp_password": "p", "from": "a@example.com", "to": []string{"b@example.com"}},
		{"name": "x", "kind": "email", "smtp_host": "smtp.example.com", "smtp_port": 587, "smtp_tls": "starttls",
			"from": "a@example.com", "to": []string{"b@example.com\r\nBcc: c@example.com"}},
	} {
		create(http.StatusUnprocessableEntity, bad)
	}

	// A webhook on a private address gets a generated secret and signs.
	hook := create(http.StatusCreated, map[string]any{"name": "ops-hook", "kind": "webhook", "url": receiver.URL + "/hook",
		"allow_private": true, "events": []string{"health.down", "health.up", "deploy.failed", "deploy.failed"}})["channel"].(map[string]any)
	secret, _ := hook["secret"].(string)
	if len(secret) != 48 || jsonString(hook["events"]) != `["deploy.failed","health.down","health.up"]` {
		t.Fatalf("hook = %v", hook)
	}
	hid := jsonID(hook["id"])
	create(http.StatusConflict, map[string]any{"name": "ops-hook", "kind": "webhook", "url": receiver.URL, "allow_private": true})
	if r := p.call(200, "POST", "/api/v1/notifications/"+hid+"/test", nil); r["ok"] != true {
		t.Fatalf("test: %v", r)
	}
	d := next("test webhook")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(d.body)
	if d.path != "/hook" || d.event != "test" || d.sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("delivery %+v", d)
	}

	// Telegram: the token never comes back and is stored encrypted.
	tg := create(http.StatusCreated, map[string]any{"name": "tg", "kind": "telegram", "token": "123456:SECRET-token",
		"chat_id": "-1001234", "events": []string{"backup.failed"}})["channel"].(map[string]any)
	tid := jsonID(tg["id"])
	if strings.Contains(jsonString(p.call(200, "GET", "/api/v1/notifications", nil)), "SECRET") {
		t.Fatal("token in the list")
	}
	var raw []byte
	p.db.R.QueryRow(`SELECT config FROM notification_channels WHERE id = ?`, tid).Scan(&raw)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "1001234") {
		t.Fatal("telegram settings stored in clear")
	}
	p.call(200, "POST", "/api/v1/notifications/"+tid+"/test", nil)
	if d := next("telegram"); d.path != "/bot123456:SECRET-token/sendMessage" || !strings.Contains(string(d.body), `"chat_id":"-1001234"`) {
		t.Fatalf("telegram %+v", d)
	}
	// Editing without a token keeps it; the kind cannot change.
	p.call(200, "PUT", "/api/v1/notifications/"+tid, map[string]any{"name": "tg2", "kind": "telegram", "chat_id": "@alerts",
		"events": []string{"backup.failed"}})
	p.call(200, "POST", "/api/v1/notifications/"+tid+"/test", nil)
	if d := next("telegram after edit"); d.path != "/bot123456:SECRET-token/sendMessage" || !strings.Contains(string(d.body), `"@alerts"`) {
		t.Fatalf("telegram %+v", d)
	}
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/notifications/"+tid, map[string]any{"name": "tg2", "kind": "webhook",
		"url": "https://example.com"})

	// A channel that cannot be reached reports why; private addresses need
	// the channel's permission.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	dead := jsonID(create(http.StatusCreated, map[string]any{"name": "dead", "kind": "webhook", "url": closedURL,
		"allow_private": true})["channel"].(map[string]any)["id"])
	if r := p.call(200, "POST", "/api/v1/notifications/"+dead+"/test", nil); r["ok"] != false || !strings.Contains(r["error"].(string), "refused") {
		t.Fatalf("dead: %v", r)
	}
	public := jsonID(create(http.StatusCreated, map[string]any{"name": "public-only", "kind": "webhook",
		"url": strings.Replace(receiver.URL, "http://", "https://", 1)})["channel"].(map[string]any)["id"])
	if r := p.call(200, "POST", "/api/v1/notifications/"+public+"/test", nil); r["ok"] != false ||
		!strings.Contains(r["error"].(string), "not a public address") {
		t.Fatalf("private address: %v", r)
	}
	for _, c := range p.call(200, "GET", "/api/v1/notifications", nil)["channels"].([]any) {
		if c := c.(map[string]any); c["name"] == "dead" && !strings.Contains(c["last_error"].(string), "refused") {
			t.Fatalf("dead channel = %v", c)
		}
	}

	// Events: a health check going down and a failed deployment.
	p.call(http.StatusCreated, "POST", "/api/v1/health-checks", map[string]any{"name": "shop", "kind": "tcp",
		"host": "127.0.0.1", "port": mustAtoi(strings.TrimPrefix(closedURL, "http://127.0.0.1:")), "interval": 2, "timeout": 1})
	d = next("health.down")
	var m struct{ Event, Title, Text string }
	json.Unmarshal(d.body, &m)
	if m.Event != "health.down" || m.Title != "shop is down" || !strings.Contains(m.Text, "refused") {
		t.Fatalf("health message %s", d.body)
	}
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, h.id); err != nil {
		t.Fatal(err)
	}
	app := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": mustAtoi(h.id), "name": "web",
		"image": "nginx:1.27", "port": 80})["app"].(map[string]any)["id"])
	p.call(http.StatusAccepted, "POST", "/api/v1/apps/"+app+"/deploy", map[string]any{})
	for {
		d = next("deploy.failed")
		if d.event == "deploy.failed" {
			break
		}
	}
	json.Unmarshal(d.body, &m)
	if !strings.HasPrefix(m.Title, "Deployment #") || !strings.HasSuffix(m.Title, " of web failed") || m.Text == "" {
		t.Fatalf("deploy message %s", d.body)
	}

	// Admins only; deleting a channel stops it.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(http.StatusForbidden, "GET", "/api/v1/notifications", nil)
	p.call(http.StatusNoContent, "DELETE", "/api/v1/notifications/"+hid, nil)
	p.call(http.StatusNotFound, "POST", "/api/v1/notifications/"+hid+"/test", nil)
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(action) FROM audit_log WHERE action LIKE 'notification.%'`).Scan(&audit)
	if !strings.HasPrefix(audit, "notification.created,notification.created,notification.updated,") ||
		!strings.HasSuffix(audit, "notification.deleted") {
		t.Fatalf("audit: %s", audit)
	}
}

// TestServerOfflineNotifications: a server whose agent stays away is
// reported once, and again when it is back; messages link to the panel.
func TestServerOfflineNotifications(t *testing.T) {
	got := make(chan map[string]any, 16)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		got <- m
	}))
	t.Cleanup(receiver.Close)
	next := func(what string) map[string]any {
		t.Helper()
		select {
		case m := <-got:
			return m
		case <-time.After(15 * time.Second):
			t.Fatalf("no delivery: %s", what)
			return nil
		}
	}

	p := startPanelWith(t, server.Options{OfflineAfter: time.Second})
	watch, stopWatch := context.WithCancel(context.Background())
	t.Cleanup(stopWatch)
	go p.srv.WatchServers(watch)
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/notifications/panel-url", map[string]string{"url": "panel.example.com"})
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/notifications/panel-url", map[string]string{"url": "https://panel.example.com/x"})
	p.call(200, "PUT", "/api/v1/notifications/panel-url", map[string]string{"url": "https://panel.example.com/"})
	if p.call(200, "GET", "/api/v1/notifications", nil)["panel_url"] != "https://panel.example.com" {
		t.Fatal("panel url not stored")
	}
	p.call(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "ops", "kind": "webhook",
		"url": receiver.URL, "allow_private": true, "events": []string{"server.offline", "server.online"}})

	created := p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "edge-1"})
	id := jsonID(created["server"].(map[string]any)["id"])
	agentDir := t.TempDir()
	if _, err := agent.Enroll(context.Background(), agent.EnrollOptions{DataDir: agentDir, PanelURL: p.agentURL,
		Token: created["enrollment"].(map[string]any)["token"].(string), CASHA256: p.hub.CAFingerprint()}); err != nil {
		t.Fatal(err)
	}
	run := func() context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		a, err := agent.New(agent.Config{DataDir: agentDir, ProxyDir: t.TempDir(), SitesRoot: t.TempDir(), HTTPPort: 80, HTTPSPort: 443},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { a.Run(ctx); close(done) }()
		eventually(t, "online", func() bool { return p.server(id)["status"] == "online" })
		return func() { cancel(); <-done }
	}
	stop := run()
	stop()
	m := next("server.offline")
	if m["event"] != "server.offline" || m["title"] != "edge-1 is offline" || m["link"] != "https://panel.example.com/servers/"+id ||
		!strings.Contains(m["text"].(string), "Last seen") {
		t.Fatalf("offline message %v", m)
	}
	// Reported once, however long it stays away.
	select {
	case m := <-got:
		t.Fatalf("repeated: %v", m)
	case <-time.After(2 * time.Second):
	}
	stop = run()
	defer stop()
	if m = next("server.online"); m["event"] != "server.online" || m["title"] != "edge-1 is back online" {
		t.Fatalf("online message %v", m)
	}
}
