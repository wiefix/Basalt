package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIServing(t *testing.T) {
	h := newTestServer(t).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// Client-side routes all get the app shell, never cached.
	for _, p := range []string{"/", "/servers", "/sites/12/sftp", "/index.html"} {
		rec := get(p)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: Cache-Control %q", p, rec.Header().Get("Cache-Control"))
		}
		if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: missing security headers", p)
		}
	}
	// A missing asset is a 404, not the HTML shell (which would hide build errors).
	if rec := get("/assets/missing-abc123.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset: %d", rec.Code)
	}
	// The API keeps returning JSON errors.
	if rec := get("/api/v1/nope"); rec.Code != 404 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("api 404: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	// Path tricks stay inside the embedded files.
	if rec := get("/../../go.mod"); strings.Contains(rec.Body.String(), "module github.com") {
		t.Fatal("served a file outside dist")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/servers", nil))
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusForbidden {
		t.Fatalf("POST to UI: %d", rec.Code)
	}
}
