// Package e2e exercises the control plane and agent together over real
// mutual TLS on loopback.
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/agenthub"
	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/store"
)

type panel struct {
	t        *testing.T
	db       *store.DB
	hub      *agenthub.Hub
	agentURL string
	api      *httptest.Server
	hc       *http.Client
	dataDir  string
	srv      *server.Server
}

func startPanel(t *testing.T) *panel { return startPanelWith(t, server.Options{}) }

// startPanelWith starts a panel; o supplies options other than the wiring.
func startPanelWith(t *testing.T, o server.Options) *panel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()

	db, err := store.Open(ctx, filepath.Join(dir, "basalt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := agenthub.LoadOrCreateCA(ctx, db, box, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	hub := agenthub.New(log, db, ca, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go hub.ServeListener(ctx, ln)
	agentURL := "https://" + ln.Addr().String()

	o.Log, o.DB, o.Box = log, db, box
	o.Hasher = auth.NewHasher(auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1}, 4)
	o.SetupToken, o.Agents, o.AgentURL = "setup", hub, agentURL
	if o.DataDir == "" {
		o.DataDir = dir
	}
	// Every response must match the API description.
	o.CheckResponses = func(msg string) { t.Errorf("%s", msg) }
	srv := server.New(o)
	hub.ServeReleases(filepath.Join(o.DataDir, "releases"))
	hub.OnACMEChallenge(srv.ACMEChallenge)
	hub.OnHealthReport(srv.HealthReport)
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)

	jar, _ := cookiejar.New(nil)
	p := &panel{t: t, db: db, hub: hub, agentURL: agentURL, api: api, hc: &http.Client{Jar: jar}, dataDir: o.DataDir, srv: srv}
	p.call(http.StatusCreated, "POST", "/api/v1/setup", map[string]string{
		"setup_token": "setup", "email": "o@example.com", "name": "O", "password": "owner password 123",
	})
	return p
}

func (p *panel) call(want int, method, path string, body any) map[string]any {
	p.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.api.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		p.t.Fatalf("%s %s: %d %v; want %d", method, path, resp.StatusCode, out, want)
	}
	return out
}

func (p *panel) server(id string) map[string]any {
	return p.call(200, "GET", "/api/v1/servers/"+id, nil)["server"].(map[string]any)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestAgentLifecycle(t *testing.T) {
	p := startPanel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Admin adds a server and receives a one-time enrollment command.
	created := p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "web-1"})
	srv := created["server"].(map[string]any)
	id := jsonID(srv["id"])
	enr := created["enrollment"].(map[string]any)
	token := enr["token"].(string)
	if srv["status"] != "pending" || !strings.Contains(enr["command"].(string), "--ca-sha256 "+p.hub.CAFingerprint()) {
		t.Fatalf("unexpected create response: %v", created)
	}

	// 2. A wrong CA pin aborts during the TLS handshake; the token is not spent.
	_, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir: t.TempDir(), PanelURL: p.agentURL, Token: token, CASHA256: strings.Repeat("ab", 32),
	})
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("enroll with wrong pin: %v", err)
	}

	// 3. Real enrollment.
	agentDir := t.TempDir()
	gotID, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir: agentDir, PanelURL: p.agentURL, Token: token, CASHA256: p.hub.CAFingerprint(),
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if gotID != mustAtoi(id) {
		t.Fatalf("enrolled as %d; want %s", gotID, id)
	}
	if info, _ := os.Stat(filepath.Join(agentDir, "agent.pem")); info.Mode().Perm() != 0o600 {
		t.Fatalf("agent.pem mode %v", info.Mode().Perm())
	}

	// 4. The token is single-use.
	if _, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir: t.TempDir(), PanelURL: p.agentURL, Token: token, CASHA256: p.hub.CAFingerprint(),
	}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("token reuse: %v", err)
	}

	// 5. The agent connects, says hello and streams metrics. A large renewal
	// window makes it rotate its certificate on connect.
	a, err := agent.New(agent.Config{
		DataDir: agentDir, ProxyDir: t.TempDir(), SitesRoot: t.TempDir(), HTTPPort: 80, HTTPSPort: 443,
		DockerSocket: fakeDocker(t, "28.1.1"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.MetricsInterval = 100 * time.Millisecond
	a.RenewBefore = 10 * 365 * 24 * time.Hour
	firstFP := certFP(t, p.db, gotID)
	agentDone := make(chan struct{})
	go func() { a.Run(ctx); close(agentDone) }()

	eventually(t, "agent online with host facts", func() bool {
		s := p.server(id)
		return s["status"] == "online" && s["hostname"] != nil && s["os"] != nil
	})
	if d := p.server(id)["docker"]; d != "28.1.1" {
		t.Fatalf("docker version = %v", d)
	}
	eventually(t, "metrics stored", func() bool {
		m := p.call(200, "GET", "/api/v1/servers/"+id+"/metrics?minutes=5", nil)["metrics"].([]any)
		return len(m) >= 2 && m[0].(map[string]any)["disk_total"].(float64) > 0
	})

	// 6. The panel can call into the agent.
	pong := p.call(200, "POST", "/api/v1/servers/"+id+"/ping", nil)
	if pong["rtt_ms"] == nil {
		t.Fatalf("ping: %v", pong)
	}

	// 7. Certificate rotation: the new certificate is recorded, the previous
	// one stays valid, and the agent reconnects with the new one.
	eventually(t, "certificate renewed", func() bool { return certFP(t, p.db, gotID) != firstFP })
	s, _ := p.db.ServerByID(ctx, gotID)
	if s.PrevCertFP != firstFP {
		t.Fatal("previous certificate not kept during rotation")
	}
	p.hub.Disconnect(gotID)
	eventually(t, "reconnect with renewed certificate", func() bool { return p.hub.Connected(gotID) })

	// 8. Removing the server revokes the agent: the stream is dropped, the
	// panel answers that the server is gone, and the agent stops trying
	// (it keeps running: its sites go on serving) and remembers it.
	p.call(http.StatusNoContent, "DELETE", "/api/v1/servers/"+id, nil)
	removed := filepath.Join(agentDir, "removed")
	eventually(t, "agent records its removal", func() bool { _, err := os.Stat(removed); return err == nil })
	if p.hub.Connected(gotID) {
		t.Fatal("removed server's agent is still connected")
	}
	select {
	case <-agentDone:
		t.Fatal("the agent stopped instead of serving on")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	<-agentDone

	// 9. Enrolling again clears the mark.
	again := p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "edge-again"})["enrollment"].(map[string]any)["token"].(string)
	if _, err := agent.Enroll(context.Background(), agent.EnrollOptions{
		DataDir: agentDir, PanelURL: p.agentURL, Token: again, CASHA256: p.hub.CAFingerprint(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Fatal("re-enrolling kept the removal mark")
	}
}

func TestConnectRequiresTrustedClientCertificate(t *testing.T) {
	p := startPanel(t)
	connect := func(cert *tls.Certificate) int {
		cfg := &tls.Config{InsecureSkipVerify: true}
		if cert != nil {
			// Force-send the certificate even if the server does not list
			// its issuer, so the server-side check is what gets tested.
			cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
		}
		var protos http.Protocols
		protos.SetHTTP2(true)
		hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, Protocols: &protos}}
		resp, err := hc.Post(p.agentURL+agentproto.PathConnect, "application/octet-stream", nil)
		if err != nil {
			return -1 // handshake rejected
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := connect(nil); code != http.StatusUnauthorized {
		t.Fatalf("no client cert: %d; want 401", code)
	}

	// A certificate from a foreign CA claiming to be server 1 is refused at
	// the TLS layer.
	evil, _ := pki.NewCA("evil", time.Now())
	key, _ := pki.NewKey()
	csr, _ := pki.NewCSR(key)
	der, _ := evil.SignAgent(csr, 1, time.Now())
	if code := connect(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}); code != -1 {
		t.Fatalf("foreign certificate: %d; want handshake failure", code)
	}
}

// fakeDocker serves the Docker Engine version endpoint on a unix socket.
func fakeDocker(t *testing.T, version string) string {
	t.Helper()
	dir, _ := os.MkdirTemp("/tmp", "bd")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.44/version" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"Version":"`+version+`","ApiVersion":"1.49"}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func certFP(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	s, err := db.ServerByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.CertFP
}

func jsonID(v any) string { return strings.TrimSuffix(strings.TrimSuffix(jsonString(v), ".0"), "\n") }

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustAtoi(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

func newJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}
