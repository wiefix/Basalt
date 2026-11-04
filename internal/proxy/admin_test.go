package proxy

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestAdminFreshConnections: Caddy replaces its admin server on every
// config load, so a kept-alive connection may lead to the old one, which
// is shutting down. Every request must use its own connection.
func TestAdminFreshConnections(t *testing.T) {
	dir, err := os.MkdirTemp("", "padm") // short: unix socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "admin.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var conns atomic.Int32
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		},
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	a := NewAdmin(socket)
	ctx := context.Background()
	if err := a.Load(ctx, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAppUpstreams(ctx, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := a.Load(ctx, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if n := conns.Load(); n != 3 {
		t.Fatalf("3 requests used %d connections, want 3", n)
	}
}
