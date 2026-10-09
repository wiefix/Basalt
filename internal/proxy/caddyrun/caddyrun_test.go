package caddyrun

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/proxy"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestProxyServes runs the real embedded Caddy.
func TestProxyServes(t *testing.T) {
	// Unix socket paths are limited to ~104 bytes; keep the directory short.
	dir, err := os.MkdirTemp("/tmp", "bp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	sites := filepath.Join(dir, "sites")
	site := filepath.Join(sites, "1")
	os.MkdirAll(filepath.Join(site, "sub"), 0o755)
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>hello</h1>"), 0o644)
	os.WriteFile(filepath.Join(site, "sub", "page.txt"), []byte("inner"), 0o644)
	os.WriteFile(filepath.Join(site, ".env"), []byte("DB_PASSWORD=secret"), 0o644)
	// Another site's secret, and a symlink pointing at it from site 1.
	os.MkdirAll(filepath.Join(sites, "2"), 0o755)
	os.WriteFile(filepath.Join(sites, "2", "config.php"), []byte("other-site-secret"), 0o644)
	os.Symlink(filepath.Join(sites, "2", "config.php"), filepath.Join(site, "leak.txt"))
	os.Symlink("sub/page.txt", filepath.Join(site, "inside.txt")) // stays within the root: allowed

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "upstream/1.0")
		io.WriteString(w, "from upstream "+r.Host)
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	defer func() { cancel(); <-done }()

	admin := proxy.NewAdmin(proxy.AdminSocket(dir))
	waitFor(t, "admin socket", func() bool { _, err := admin.Running(ctx, "x"); return err == nil })

	httpPort, httpsPort := freePort(t), freePort(t)
	d := proxy.Desired{Routes: []proxy.Route{
		{ID: 1, Kind: proxy.KindStatic, Domains: []string{"site.localhost"}, Root: site, HSTS: true},
		{ID: 2, Kind: proxy.KindReverseProxy, Domains: []string{"app.localhost"}, Upstream: strings.TrimPrefix(upstream.URL, "http://")},
	}}
	if err := d.Validate(sites); err != nil {
		t.Fatal(err)
	}
	cfg, id, err := proxy.Build(d, proxy.BuildOptions{Dir: dir, HTTPPort: httpPort, HTTPSPort: httpsPort})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Load(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if ok, err := admin.Running(ctx, id); !ok || err != nil {
		t.Fatalf("Running(%s) = %v, %v", id, ok, err)
	}

	addr := "127.0.0.1:" + strconv.Itoa(httpsPort)
	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // internal CA
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(url string) (int, http.Header, string) {
		t.Helper()
		var resp *http.Response
		waitFor(t, "response from "+url, func() bool {
			var err error
			resp, err = hc.Get(url)
			return err == nil
		})
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}

	code, hdr, body := get("https://site.localhost/")
	if code != 200 || body != "<h1>hello</h1>" {
		t.Fatalf("index: %d %q", code, body)
	}
	if hdr.Get("Server") != "" || hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Strict-Transport-Security") == "" {
		t.Fatalf("headers: %v", hdr)
	}
	if code, _, body := get("https://site.localhost/sub/page.txt"); code != 200 || body != "inner" {
		t.Fatalf("subdir: %d %q", code, body)
	}
	if code, _, body := get("https://site.localhost/inside.txt"); code != 200 || body != "inner" {
		t.Fatalf("symlink within root: %d %q", code, body)
	}

	// Attacks: none of these may return the secrets.
	for _, path := range []string{"/leak.txt", "/.env", "/../2/config.php", "/%2e%2e/2/config.php", "/sub/../../2/config.php"} {
		code, _, body := get("https://site.localhost" + path)
		if strings.Contains(body, "secret") || code == 200 {
			t.Errorf("%s: %d %q — must not be served", path, code, body)
		}
	}

	code, hdr, body = get("https://app.localhost/")
	if code != 200 || body != "from upstream app.localhost" || hdr.Get("Server") != "" {
		t.Fatalf("reverse proxy: %d %q server=%q", code, body, hdr.Get("Server"))
	}

	// Plain HTTP redirects to HTTPS.
	plain := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(httpPort)+"/x", nil)
	req.Host = "site.localhost"
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || !strings.HasPrefix(resp.Header.Get("Location"), "https://site.localhost") {
		t.Fatalf("http: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// A config with an invalid route is rejected and the old one keeps serving.
	if err := admin.Load(ctx, []byte(`{"apps":{"http":{"servers":{"x":{"listen":["bogus"]}}}}}`)); err == nil {
		t.Fatal("invalid config accepted")
	}
	if code, _, _ := get("https://site.localhost/"); code != 200 {
		t.Fatal("previous config lost after a failed load")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
