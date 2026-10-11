package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/proxy/caddyrun"
)

// acmeRelay answers the proxy's DNS-01 requests on a socket in the proxy
// directory (owned by the proxy user, 0600) by forwarding them to the
// panel, which publishes the records on every DNS server.
type acmeRelay struct {
	log    *slog.Logger
	socket string
	dir    string

	mu    sync.Mutex
	panel *agentproto.Peer // nil while disconnected
}

func newACMERelay(cfg Config, log *slog.Logger) *acmeRelay {
	return &acmeRelay{log: log, socket: proxy.ACMESocket(cfg.ProxyDir), dir: cfg.ProxyDir}
}

func (r *acmeRelay) setPanel(p *agentproto.Peer) {
	r.mu.Lock()
	r.panel = p
	r.mu.Unlock()
}

func (r *acmeRelay) run(ctx context.Context) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		r.log.Error("acme relay disabled", "err", err)
		return
	}
	if err := os.Remove(r.socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.log.Error("acme relay disabled", "err", err)
		return
	}
	ln, err := net.Listen("unix", r.socket)
	if err != nil {
		r.log.Error("acme relay disabled", "err", err)
		return
	}
	// Only the proxy may ask: the socket belongs to the owner of its directory.
	if err := os.Chmod(r.socket, 0o600); err == nil {
		err = chownLike(r.socket, r.dir)
	}
	if err != nil {
		ln.Close()
		r.log.Error("acme relay disabled", "err", err)
		return
	}
	srv := &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 3 * time.Minute}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.log.Error("acme relay stopped", "err", err)
	}
}

func (r *acmeRelay) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != caddyrun.ChallengePath || (req.Method != http.MethodPost && req.Method != http.MethodDelete) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var c caddyrun.ChallengeRequest
	if err := json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&c); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSuffix(strings.ToLower(c.Name), ".")
	if !strings.HasPrefix(name, "_acme-challenge.") || len(c.Value) == 0 || len(c.Value) > 255 {
		http.Error(w, "only _acme-challenge TXT records can be set", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	panel := r.panel
	r.mu.Unlock()
	if panel == nil {
		http.Error(w, "the agent is not connected to the panel", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
	defer cancel()
	msg := agentproto.ACMEChallenge{Name: name, Value: c.Value, Present: req.Method == http.MethodPost}
	if err := panel.Call(ctx, agentproto.TypeACMEChallenge, msg, nil); err != nil {
		r.log.Warn("acme challenge", "name", name, "present", msg.Present, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	r.log.Info("acme challenge", "name", name, "present", msg.Present)
	w.WriteHeader(http.StatusNoContent)
}
