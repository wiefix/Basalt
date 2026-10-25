package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wiefix/basalt/web"
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

// TestUIGzip: compressible files go gzipped to clients that take gzip,
// unchanged to the others, and say they vary by Accept-Encoding.
func TestUIGzip(t *testing.T) {
	dist, _ := fs.Sub(web.Dist, "dist")
	var asset string
	fs.WalkDir(dist, "assets", func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".js") && asset == "" {
			asset = p
		}
		return nil
	})
	if asset == "" {
		t.Skip("the web UI is not built")
	}
	raw, _ := fs.ReadFile(dist, asset)
	h := newTestServer(t).Handler()
	get := func(p, enc string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, p := range []string{"/" + asset, "/", "/servers"} {
		rec := get(p, "br, gzip;q=0.8")
		if rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s: encoding %q vary %q", p, rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		if p == "/"+asset && !bytes.Equal(b, raw) {
			t.Fatal("the gzipped asset differs")
		}
		if p != "/"+asset && !strings.Contains(string(b), "<!doctype html") && !strings.Contains(string(b), "<!DOCTYPE html") {
			t.Fatalf("%s: not the app shell", p)
		}
	}
	if rec := get("/"+asset, ""); rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatal("served gzipped to a client that did not ask")
	}
	if rec := get("/"+asset, "gzip;q=0, identity"); rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("served gzipped to a client that refused it")
	}
	if rec := get("/"+asset, "gzip"); rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
	}
}
