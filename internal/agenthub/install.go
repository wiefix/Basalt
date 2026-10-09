package agenthub

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var platformRE = regexp.MustCompile(`^linux-(amd64|arm64)$`)

// ServeInstaller exposes, on the agent listener, what a new host needs to
// install itself: the CA certificate (to be checked against the fingerprint
// in the install command), the install script and the agent binary. All but
// the certificate are fetched by the script with that verified CA, so
// nothing executed on the host comes over an unauthenticated channel.
func (h *Hub) ServeInstaller(script []byte, binariesDir string) {
	h.installScript = script
	h.binariesDir = binariesDir
}

func (h *Hub) registerInstaller(mux *http.ServeMux) {
	mux.HandleFunc("GET /agent/v1/ca.der", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pkix-cert")
		w.Write(h.ca.Cert.Raw)
	})
	mux.HandleFunc("GET /agent/v1/install.sh", func(w http.ResponseWriter, r *http.Request) {
		if len(h.installScript) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Write(h.installScript)
	})
	mux.HandleFunc("GET /agent/v1/binary/{platform}", func(w http.ResponseWriter, r *http.Request) {
		platform := r.PathValue("platform")
		if !platformRE.MatchString(platform) {
			http.Error(w, "unsupported platform", http.StatusNotFound)
			return
		}
		path := ""
		if h.binariesDir != "" {
			candidate := filepath.Join(h.binariesDir, "basalt-"+platform)
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
			}
		}
		if path == "" && platform == runtime.GOOS+"-"+runtime.GOARCH {
			path, _ = os.Executable()
		}
		if path == "" {
			http.Error(w, "no basalt build for "+platform+" on this panel; put basalt-"+platform+" in --binaries-dir",
				http.StatusNotFound)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, "binary unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "basalt-"+strings.TrimPrefix(platform, "linux-"), st.ModTime(), f)
	})
}
