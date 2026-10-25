package server

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/web"
)

const uiMissing = `<!doctype html><meta charset="utf-8"><title>Basalt</title>
<body style="font-family:system-ui;max-width:36rem;margin:4rem auto;padding:0 1rem">
<h1>Web UI not built</h1><p>The API is running. Build the interface with <code>make web</code> and rebuild the binary.</p>`

// compressible are the kinds of files served gzipped (images and fonts
// are compressed already).
var compressible = map[string]bool{".js": true, ".css": true, ".html": true, ".svg": true, ".json": true, ".txt": true, ".map": true}

// gzipped holds the gzipped files of dist, each compressed on its first
// request (nil: it does not shrink).
type gzipped struct {
	dist fs.FS
	mu   sync.Mutex
	m    map[string][]byte
}

func (g *gzipped) get(name string) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.m[name]; ok {
		return b
	}
	var out []byte
	if b, err := fs.ReadFile(g.dist, name); err == nil && len(b) >= 256 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(b)
		zw.Close()
		if buf.Len() < len(b) {
			out = buf.Bytes()
		}
	}
	g.m[name] = out
	return out
}

// acceptsGzip reads Accept-Encoding: gzip (or *) without q=0.
func acceptsGzip(h string) bool {
	for _, part := range strings.Split(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		q := 1.0
		for _, prm := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(prm), "="); ok && strings.TrimSpace(k) == "q" {
				q, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			}
		}
		if q > 0 {
			return true
		}
	}
	return false
}

// serveGzipped answers with the gzipped version of name if there is one and
// the client takes it.
func serveGzipped(w http.ResponseWriter, r *http.Request, gz *gzipped, name string, mod time.Time) bool {
	if !compressible[path.Ext(name)] {
		return false
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		return false
	}
	b := gz.get(name)
	if b == nil {
		return false
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if path.Ext(name) == ".html" {
		ct = "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Encoding", "gzip")
	r.Header.Del("Range") // ranges of the encoded bytes would confuse clients
	http.ServeContent(w, r, name, mod, bytes.NewReader(b))
	return true
}

// uiHandler serves the single-page app, gzipped to clients that take it.
// Hashed assets are cached forever; every other path returns index.html so
// client-side routes work on reload.
func uiHandler() http.Handler {
	dist, _ := fs.Sub(web.Dist, "dist")
	index, err := fs.ReadFile(dist, "index.html")
	gz := &gzipped{dist: dist, m: map[string][]byte{}}
	if err != nil {
		index = []byte(uiMissing)
		gz.m["index.html"] = nil // the page above is not in dist
	}
	start := time.Now()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if f, err := dist.Open(name); err == nil {
				defer f.Close()
				if st, err := f.Stat(); err == nil && !st.IsDir() {
					if strings.HasPrefix(name, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "public, max-age=3600")
					}
					if serveGzipped(w, r, gz, name, start) {
						return
					}
					http.ServeContent(w, r, name, start, f.(interface {
						fs.File
						Read([]byte) (int, error)
						Seek(int64, int) (int64, error)
					}))
					return
				}
			}
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r) // never answer a missing asset with HTML
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		if serveGzipped(w, r, gz, "index.html", start) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", start, bytes.NewReader(index))
	})
}
