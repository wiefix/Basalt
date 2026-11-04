package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wiefix/basalt/internal/proxy"
)

// TestProxyUnreadableState: a saved state the agent cannot read (left by an
// agent that ran as root, or damaged) must not replace what the proxy runs
// with the panel's route alone; a missing one (a new server) still
// publishes the panel.
func TestProxyUnreadableState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved string // "" — no file
		loads int32
	}{
		{"missing", "", 1},
		{"unreadable", "{broken", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "pxy") // short: unix socket path limit
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			ln, err := net.Listen("unix", proxy.AdminSocket(dir))
			if err != nil {
				t.Fatal(err)
			}
			var loads atomic.Int32
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				switch {
				case r.URL.Path == "/load":
					loads.Add(1)
				case strings.HasPrefix(r.URL.Path, "/id/"):
					http.NotFound(w, r) // not running the wanted config
				}
			})}
			go srv.Serve(ln)
			t.Cleanup(func() { srv.Close() })

			state := filepath.Join(dir, fileProxyDesired)
			if tc.saved != "" {
				if err := os.WriteFile(state, []byte(tc.saved), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m := &proxyManager{
				log:       slog.New(slog.DiscardHandler),
				stateFile: state,
				opts:      proxy.BuildOptions{Dir: dir, HTTPPort: 80, HTTPSPort: 443},
				admin:     proxy.NewAdmin(proxy.AdminSocket(dir)),
				panel: &proxy.Route{Kind: proxy.KindReverseProxy, Domains: []string{"panel.example.com"},
					Upstream: "127.0.0.1:8080", TLS: proxy.TLSAuto},
			}
			if err := m.loadSaved(); (err != nil) != (tc.saved != "") {
				t.Fatalf("loadSaved: %v", err)
			}
			m.reconcile(context.Background())
			if n := loads.Load(); n != tc.loads {
				t.Fatalf("proxy loaded %d configs, want %d", n, tc.loads)
			}
		})
	}
}
