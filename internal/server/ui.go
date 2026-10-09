package server

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/wiefix/basalt/web"
)

const uiMissing = `<!doctype html><meta charset="utf-8"><title>Basalt</title>
<body style="font-family:system-ui;max-width:36rem;margin:4rem auto;padding:0 1rem">
<h1>Web UI not built</h1><p>The API is running. Build the interface with <code>make web</code> and rebuild the binary.</p>`

// uiHandler serves the single-page app. Hashed assets are cached forever;
// every other path returns index.html so client-side routes work on reload.
func uiHandler() http.Handler {
	dist, _ := fs.Sub(web.Dist, "dist")
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		index = []byte(uiMissing)
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", start, bytes.NewReader(index))
	})
}
