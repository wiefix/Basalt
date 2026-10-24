package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/standby"
)

// TestAutoFailover: the owner lets a standby take over by itself. Cut off
// from the panel while a server still reaches it, it does not; when the
// panel is gone and the server confirms it, it takes over, and the
// server's agent moves to it.
func TestAutoFailover(t *testing.T) {
	a := startPanelWith(t, server.Options{FailoverMinAfter: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A server whose agent answers through its tunnel: the witness.
	created := a.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "web"})
	sid := mustAtoi(jsonID(created["server"].(map[string]any)["id"]))
	agentDir := t.TempDir()
	if _, err := agent.Enroll(ctx, agent.EnrollOptions{DataDir: agentDir, PanelURL: a.agentURL,
		Token: created["enrollment"].(map[string]any)["token"].(string), CASHA256: a.hub.CAFingerprint()}); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Config{DataDir: agentDir, ProxyDir: t.TempDir(), SitesRoot: t.TempDir(), HTTPPort: 80, HTTPSPort: 443,
		PeerListen: "127.0.0.1:0"}, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { stopAgent(); <-agentDone }()
	eventually(t, "the agent online", func() bool { return a.hub.Connected(sid) })
	a.call(200, "PUT", "/api/v1/servers/"+jsonID(created["server"].(map[string]any)["id"])+"/public-ips", map[string]any{"ips": []string{"127.0.0.1"}})

	// The standby.
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bURL := "https://" + lnB.Addr().String()
	r := a.call(http.StatusCreated, "POST", "/api/v1/standbys", map[string]any{"name": "spare"})
	sbID := jsonID(r["standby"].(map[string]any)["id"])
	dir := t.TempDir()
	if _, err := standby.Enroll(ctx, standby.EnrollOptions{DataDir: dir, PanelURL: a.agentURL,
		Token: r["enrollment"].(map[string]any)["token"].(string), CASHA256: a.hub.CAFingerprint()}); err != nil {
		t.Fatal(err)
	}
	run := func() (*standby.Client, chan error, func()) {
		c, err := standby.NewClient(dir, bURL, log)
		if err != nil {
			t.Fatal(err)
		}
		unlock, err := c.Lock()
		if err != nil {
			t.Fatal(err)
		}
		sctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- c.Run(sctx); unlock() }()
		return c, done, func() { stop(); <-done }
	}
	c, _, stop := run()
	eventually(t, "the copy is complete", func() bool { return c.Status().Ready })

	// The setting: checked, then on.
	body := func(id string, after int) map[string]any {
		return map[string]any{"standby_id": mustAtoi(id), "after_s": after}
	}
	a.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/standbys/failover", body(sbID, 0))
	a.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/standbys/failover", body("999", 60))
	eventually(t, "the agent's tunnel known", func() bool {
		for _, s := range a.call(200, "GET", "/api/v1/servers", nil)["servers"].([]any) {
			if s := s.(map[string]any); jsonID(s["id"]) == jsonID(float64(sid)) && s["peer_port"] != nil && s["peer_port"] != 0.0 {
				return true
			}
		}
		return false
	})
	fo := a.call(200, "PUT", "/api/v1/standbys/failover", body(sbID, 2))["failover"].(map[string]any)
	if ws := fo["witnesses"].([]any); len(ws) != 1 || ws[0].(map[string]any)["name"] != "web" {
		t.Fatalf("witnesses: %v", fo)
	}
	if l := a.call(200, "GET", "/api/v1/standbys", nil)["failover"].(map[string]any); l["standby_id"] != fo["standby_id"] || l["after_s"] != 2.0 {
		t.Fatalf("failover: %v", l)
	}
	eventually(t, "the standby learns it takes over by itself", func() bool { return c.Status().AutoFailover && c.Status().AfterS == 2 })

	// Cut off from the panel (a wrong address) while the server reaches
	// it: the standby asks, and stays a standby.
	stop()
	cfgPath := filepath.Join(dir, standby.FileConfig)
	orig, _ := os.ReadFile(cfgPath)
	var m map[string]any
	json.Unmarshal(orig, &m)
	m["panel_url"], m["panels"] = "https://127.0.0.1:9", []string{"https://127.0.0.1:9"}
	cut, _ := json.Marshal(m)
	os.WriteFile(cfgPath, cut, 0o600)
	c, done, stop := run()
	eventually(t, "the standby asked the server", func() bool { return strings.Contains(c.Status().Note, "reaches the panel") })
	select {
	case err := <-done:
		t.Fatalf("the standby stopped: %v", err)
	case <-time.After(3 * time.Second):
	}
	stop()
	os.WriteFile(cfgPath, orig, 0o600)
	c, done, _ = run()
	eventually(t, "following the panel again", func() bool { return c.Status().Connected })

	// The panel is gone: the server confirms, the standby takes over.
	a.stop()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the standby did not take over")
	}
	if !errors.Is(runErr, standby.ErrTakeOver) {
		t.Fatalf("run: %v", runErr)
	}
	p, err := standby.TakeOver(ctx, dir)
	if err != nil || p.Epoch != 1 {
		t.Fatalf("take over: %+v %v", p, err)
	}
	b := openPanel(t, server.Options{}, dir, lnB)
	b.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "o@example.com", "password": "owner password 123"})
	eventually(t, "the agent moves to the new panel", func() bool { return b.hub.Connected(sid) })
	if p := b.call(200, "GET", "/api/v1/panel", nil)["panel"].(map[string]any); p["epoch"] != 1.0 {
		t.Fatalf("new panel: %v", p)
	}
	// The new panel no longer has a standby taking over.
	if f := b.call(200, "GET", "/api/v1/standbys", nil)["failover"].(map[string]any); f["standby_id"] != 0.0 {
		t.Fatalf("failover on the new panel: %v", f)
	}
}
