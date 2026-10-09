package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWith(t, Options{SetupToken: testSetupToken})
}

const testSetupToken = "test-setup-token"

func newTestServerWith(t *testing.T, o Options) *Server {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "basalt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secrets.NewBox(bytes.Repeat([]byte{1}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	o.DB = db
	o.Box = box
	o.Hasher = auth.NewHasher(auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1}, 4)
	return New(o)
}

func TestHealthz(t *testing.T) {
	h := newTestServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Fatalf("body = %q, %v", rec.Body.String(), err)
	}
}

func TestHealthzReportsDatabaseFailure(t *testing.T) {
	s := newTestServer(t)
	s.db.Close()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, path := range []string{"/healthz", "/api/v1/version", "/api/v1/missing", "/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
			if rec.Header().Get(name) == "" {
				t.Errorf("%s: missing %s header", path, name)
			}
		}
	}
}

func TestUnknownAPIRouteIsJSON404(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestWrongMethodRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", rec.Code)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	s := newTestServer(t)
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
}

func TestGracefulShutdown(t *testing.T) {
	s := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRealIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("127.0.0.1, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	h := realIP(trusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = clientIP(r) }))
	for _, c := range []struct{ peer, xff, want string }{
		{"127.0.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{"127.0.0.1:5000", "6.6.6.6, 203.0.113.9", "203.0.113.9"},  // left entry is client-supplied
		{"127.0.0.1:5000", "203.0.113.9, 10.1.2.3", "203.0.113.9"}, // skip inner trusted hop
		{"198.51.100.7:5000", "1.2.3.4", "198.51.100.7"},           // untrusted peer: header ignored
		{"127.0.0.1:5000", "", "127.0.0.1"},                        // no header
		{"127.0.0.1:5000", "garbage, 203.0.113.9", "203.0.113.9"},  // garbage left of a valid hop
		{"127.0.0.1:5000", "203.0.113.9, garbage", "127.0.0.1"},    // malformed last hop: keep peer
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("peer %s xff %q: got %s want %s", c.peer, c.xff, got, c.want)
		}
	}
	if _, err := ParseTrustedProxies("nope"); err == nil {
		t.Error("invalid proxy accepted")
	}
}
