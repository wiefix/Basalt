package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/proxy/caddyrun"
)

// TestRoutesReachTheProxy drives the whole chain: panel API -> agent over
// mutual TLS -> embedded Caddy -> HTTPS response.
func TestRoutesReachTheProxy(t *testing.T) {
	p := startPanel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Short path: unix socket names are limited to ~104 bytes.
	proxyDir, _ := os.MkdirTemp("/tmp", "bp")
	defer os.RemoveAll(proxyDir)
	sitesRoot := t.TempDir()
	siteDir := filepath.Join(sitesRoot, "1", "public")
	os.MkdirAll(siteDir, 0o755)
	os.WriteFile(filepath.Join(siteDir, "index.html"), []byte("site one"), 0o644)

	startProxy := func() (stop func()) {
		pctx, pcancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- caddyrun.Run(pctx, proxyDir, quiet) }()
		return func() { pcancel(); <-done }
	}
	stopProxy := startProxy()
	defer func() { stopProxy() }()

	created := p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "edge"})
	id := jsonID(created["server"].(map[string]any)["id"])
	agentDir := t.TempDir()
	if _, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir: agentDir, PanelURL: p.agentURL,
		Token:    created["enrollment"].(map[string]any)["token"].(string),
		CASHA256: p.hub.CAFingerprint(),
	}); err != nil {
		t.Fatal(err)
	}
	httpsPort := freePort(t)
	a, err := agent.New(agent.Config{
		DataDir: agentDir, ProxyDir: proxyDir, SitesRoot: sitesRoot,
		HTTPPort: freePort(t), HTTPSPort: httpsPort,
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() { a.Run(agentCtx); close(agentDone) }()
	// Online comes with the connection, the host's facts with its hello a
	// moment later (hostname is written with them).
	eventually(t, "agent online", func() bool { s := p.server(id); return s["status"] == "online" && s["hostname"] != nil })

	routes := "/api/v1/servers/" + id + "/routes"
	addRoute := func(body map[string]any) (routeID string, sync map[string]any) {
		r := p.call(http.StatusCreated, "POST", routes, body)
		return jsonID(r["route"].(map[string]any)["id"]), r["sync"].(map[string]any)
	}
	get := httpsGetter(t, httpsPort)
	served := func(host, want string) func() bool {
		return func() bool { code, body := get(host, "/"); return code == 200 && body == want }
	}

	// 1. A static site is live over HTTPS right after it is created.
	if _, sync := addRoute(map[string]any{
		"kind": "static", "domains": []string{"Site.localhost"}, "root": siteDir, "tls": "internal",
	}); sync["in_sync"] != true {
		t.Fatalf("route not applied: %v", sync)
	}
	// The certificate is issued asynchronously right after the config loads.
	eventually(t, "site served over HTTPS", served("site.localhost", "site one"))

	// 2. A domain can be routed only once per server.
	p.call(http.StatusConflict, "POST", routes, map[string]any{
		"kind": "reverse_proxy", "domains": []string{"site.localhost"}, "upstream": "127.0.0.1:1",
	})

	// 3. A root outside the agent's sites directory is refused by the agent;
	// the panel records the error and the proxy keeps the last good config.
	badID, sync := addRoute(map[string]any{
		"kind": "static", "domains": []string{"evil.localhost"}, "root": "/etc", "tls": "internal",
	})
	if sync["in_sync"] != false || !strings.Contains(fmt.Sprint(sync["error"]), "outside the sites directory") {
		t.Fatalf("expected agent to refuse /etc: %v", sync)
	}
	if !served("site.localhost", "site one")() {
		t.Fatal("good site broken by a rejected update")
	}
	if r := p.call(200, "DELETE", routes+"/"+badID, nil); r["sync"].(map[string]any)["in_sync"] != true {
		t.Fatalf("not back in sync after removing the bad route: %v", r)
	}

	// 4. A reverse proxy route, then its removal.
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app")
	})}
	uln, _ := net.Listen("tcp", "127.0.0.1:0")
	go upstream.Serve(uln)
	defer upstream.Close()
	appID, _ := addRoute(map[string]any{
		"kind": "reverse_proxy", "domains": []string{"app.localhost"}, "upstream": uln.Addr().String(),
	})
	eventually(t, "app served", served("app.localhost", "app"))
	p.call(200, "DELETE", routes+"/"+appID, nil)
	eventually(t, "app removed", func() bool { code, _ := get("app.localhost", "/"); return code != 200 })

	// 5. The proxy restarts while the agent and panel are down: it loads the
	// last applied configuration from disk on its own.
	stopAgent()
	<-agentDone
	p.api.Close()
	stopProxy()
	stopProxy = startProxy()
	eventually(t, "site served after proxy restart without agent", served("site.localhost", "site one"))
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// httpsGetter returns a function that requests https://host/path from the
// local proxy. Connection errors (e.g. no certificate yet) are status 0.
func httpsGetter(t *testing.T, port int) func(host, path string) (int, string) {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // proxy's internal CA
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
	return func(host, path string) (int, string) {
		resp, err := hc.Get("https://" + host + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
}
