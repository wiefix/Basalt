package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/standby"
)

// TestPanelFailover: agents learn where the panel's standby is; when the
// panel is gone the standby is promoted and they move to it by
// themselves, and the former panel, back again, gets no agent: it learns
// it was replaced.
func TestPanelFailover(t *testing.T) {
	a := startPanel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A server with its agent.
	created := a.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "web"})
	sid := jsonID(created["server"].(map[string]any)["id"])
	agentDir := t.TempDir()
	if _, err := agent.Enroll(ctx, agent.EnrollOptions{DataDir: agentDir, PanelURL: a.agentURL,
		Token: created["enrollment"].(map[string]any)["token"].(string), CASHA256: a.hub.CAFingerprint()}); err != nil {
		t.Fatal(err)
	}
	runAgent := func() context.CancelFunc {
		ag, err := agent.New(agent.Config{DataDir: agentDir, ProxyDir: t.TempDir(), SitesRoot: t.TempDir(), HTTPPort: 80, HTTPSPort: 443}, log)
		if err != nil {
			t.Fatal(err)
		}
		actx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { ag.Run(actx); close(done) }()
		return func() { stop(); <-done }
	}
	stopAgent := runAgent()
	defer func() { stopAgent() }()
	eventually(t, "the agent online", func() bool { return a.hub.Connected(mustAtoi(sid)) })
	if p := a.call(200, "GET", "/api/v1/panel", nil)["panel"].(map[string]any); p["epoch"] != 0.0 || p["superseded"] != nil {
		t.Fatalf("panel: %v", p)
	}

	// A standby, whose agent port will be on lnB.
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bURL := "https://" + lnB.Addr().String()
	enr := a.call(http.StatusCreated, "POST", "/api/v1/standbys", map[string]any{"name": "spare"})["enrollment"].(map[string]any)
	dir := t.TempDir()
	if _, err := standby.Enroll(ctx, standby.EnrollOptions{DataDir: dir, PanelURL: a.agentURL, Token: enr["token"].(string),
		CASHA256: a.hub.CAFingerprint()}); err != nil {
		t.Fatal(err)
	}
	if _, err := standby.Promote(ctx, dir); err == nil {
		t.Fatal("promoted without a copy")
	}
	c, err := standby.NewClient(dir, bURL, log)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := c.Lock()
	if err != nil {
		t.Fatal(err)
	}
	sctx, stopStandby := context.WithCancel(ctx)
	standbyDone := make(chan struct{})
	go func() { c.Run(sctx); close(standbyDone) }()
	eventually(t, "the standby's copy is complete, its address known", func() bool {
		l := a.call(200, "GET", "/api/v1/standbys", nil)["standbys"].([]any)
		sb := l[0].(map[string]any)
		return sb["ready"] == true && sb["agent_url"] == bURL
	})
	agentCfg := func() (cfg struct {
		PanelURL string   `json:"panel_url"`
		Panels   []string `json:"panels"`
		Epoch    int64    `json:"epoch"`
	}) {
		b, _ := os.ReadFile(filepath.Join(agentDir, "agent.json"))
		json.Unmarshal(b, &cfg)
		return cfg
	}
	eventually(t, "the agent learns the standby's address", func() bool {
		return slices.Equal(agentCfg().Panels, []string{a.agentURL, bURL})
	})
	if _, err := standby.Promote(ctx, dir); err == nil {
		t.Fatal("promoted while the standby runs")
	}
	a.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "before.test"})
	applied := c.Status().Applied
	eventually(t, "the copy takes the change", func() bool { return c.Status().Applied.After(applied) })

	// The panel fails; the standby is promoted and started as the panel.
	aAddr := agentAddr(a)
	a.stop()
	stopStandby()
	<-standbyDone
	unlock()
	pr, err := standby.Promote(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Epoch != 1 || pr.LastChange.IsZero() {
		t.Fatalf("promoted: %+v", pr)
	}
	if standby.IsStandby(dir) {
		t.Fatal("still a standby")
	}
	b := openPanel(t, server.Options{}, dir, lnB)
	b.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "o@example.com", "password": "owner password 123"})
	id := mustAtoi(sid)
	eventually(t, "the agent moves to the new panel", func() bool {
		cfg := agentCfg()
		return b.hub.Connected(id) && cfg.PanelURL == bURL && cfg.Epoch == 1 && slices.Equal(cfg.Panels, []string{bURL})
	})
	if l := b.call(200, "GET", "/api/v1/standbys", nil)["standbys"].([]any); len(l) != 0 {
		t.Fatalf("the new panel lists itself as a standby: %v", l)
	}
	zones := b.call(200, "GET", "/api/v1/dns", nil)["zones"].([]any)
	if len(zones) != 1 || zones[0].(map[string]any)["name"] != "before.test" {
		t.Fatalf("zones: %v", zones)
	}
	if p := b.call(200, "GET", "/api/v1/panel", nil)["panel"].(map[string]any); p["epoch"] != 1.0 || p["superseded"] != nil {
		t.Fatalf("new panel: %v", p)
	}

	// The former panel comes back where agents used to find it; the agent
	// still tries it first, is refused, and stays with the new panel.
	stopAgent()
	lnA, err := net.Listen("tcp", aAddr)
	if err != nil {
		t.Fatal(err)
	}
	a2 := openPanel(t, server.Options{}, a.dataDir, lnA)
	a2.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "o@example.com", "password": "owner password 123"})
	cfgPath := filepath.Join(agentDir, "agent.json")
	raw, _ := os.ReadFile(cfgPath)
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["panel_url"] = a.agentURL
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stopAgent = runAgent()
	eventually(t, "the former panel learns it was replaced", func() bool {
		p := a2.call(200, "GET", "/api/v1/panel", nil)["panel"].(map[string]any)
		s, _ := p["superseded"].(map[string]any)
		return s != nil && s["epoch"] == 1.0
	})
	eventually(t, "the agent is back on the new panel", func() bool {
		return agentCfg().PanelURL == bURL && b.hub.Connected(id)
	})
	if a2.hub.Connected(id) {
		t.Fatal("the former panel took the agent")
	}
}

// agentAddr is the address of a panel's agent port.
func agentAddr(p *panel) string { return p.agentURL[len("https://"):] }
