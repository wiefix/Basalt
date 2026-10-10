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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/proxy"
)

// TestAppUpstreamsSwitchWithoutReload moves an app between two backends
// many times under load. The switch goes through the upstream table, not a
// config reload, so no request may fail; the table survives a restart.
func TestAppUpstreamsSwitchWithoutReload(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	backend := func(name string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) }))
		t.Cleanup(s.Close)
		return strings.TrimPrefix(s.URL, "http://")
	}
	blue, green := backend("blue"), backend("green")

	start := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
		return func() { cancel(); <-done }
	}
	stop := start()
	defer func() { stop() }()
	ctx := context.Background()
	admin := proxy.NewAdmin(proxy.AdminSocket(dir))
	waitFor(t, "admin socket", func() bool { _, err := admin.Running(ctx, "x"); return err == nil })

	httpsPort := freePort(t)
	d := proxy.Desired{Routes: []proxy.Route{{ID: 1, Kind: proxy.KindReverseProxy, AppID: 7, Domains: []string{"app.localhost"}, TLS: proxy.TLSInternal}}}
	if err := d.Validate(""); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := proxy.Build(d, proxy.BuildOptions{Dir: dir, HTTPPort: freePort(t), HTTPSPort: httpsPort})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Load(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxy.ConfigPath(dir), cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:" + strconv.Itoa(httpsPort)
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true, // a new connection per request: the worst case for listener churn
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, addr)
		},
	}}
	get := func() (int, string) {
		r, err := hc.Get("https://app.localhost/")
		if err != nil {
			return 0, err.Error()
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}

	// No container yet: 503 after the short retry window.
	waitFor(t, "503 without upstream", func() bool { code, _ := get(); return code == http.StatusServiceUnavailable })
	if err := admin.SetAppUpstreams(ctx, map[int64]string{7: blue}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "blue", func() bool { _, b := get(); return b == "blue" })
	if err := admin.SetAppUpstreams(ctx, map[int64]string{7: "not-an-ip:80"}); err == nil {
		t.Fatal("invalid upstream accepted")
	}

	var ok, bad atomic.Int64
	var first atomic.Value
	halt, fin := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(fin)
		for {
			select {
			case <-halt:
				return
			default:
			}
			if code, body := get(); code == 200 && (body == "blue" || body == "green") {
				ok.Add(1)
			} else {
				bad.Add(1)
				first.CompareAndSwap(nil, strconv.Itoa(code)+" "+body)
			}
		}
	}()
	for i := range 30 {
		up := blue
		if i%2 == 0 {
			up = green
		}
		if err := admin.SetAppUpstreams(ctx, map[int64]string{7: up}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(halt)
	<-fin
	t.Logf("%d requests during 30 switches, %d failed", ok.Load(), bad.Load())
	if bad.Load() != 0 || ok.Load() == 0 {
		t.Fatalf("%d of %d requests failed; first: %v", bad.Load(), ok.Load()+bad.Load(), first.Load())
	}

	// The last switch (to blue) is kept across a proxy restart.
	stop()
	stop = start()
	waitFor(t, "served after restart", func() bool { _, b := get(); return b == "blue" })
}
