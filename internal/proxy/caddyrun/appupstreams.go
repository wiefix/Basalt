package caddyrun

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"

	"github.com/wiefix/basalt/internal/proxy"
)

func init() {
	caddy.RegisterModule(AppUpstreams{})
	caddy.RegisterModule(adminAPI{})
}

// App upstreams live outside the Caddy configuration. Moving an app to a
// new container must not reload the config: a reload replaces the
// listeners, and connections waiting in the old listener's accept queue
// are reset. The agent updates this table through the admin API instead,
// and the proxy persists it so it survives restarts without the agent.
// Keys are proxy.AppUpstreamKey: an app's published service, or another
// of its services with domains of its own.
var appTable = struct {
	sync.RWMutex
	m    map[string]string
	file string
}{m: map[string]string{}}

// AppUpstreams is a dynamic upstream source returning the current
// container of one app (or of one of its services).
type AppUpstreams struct {
	AppID   int64  `json:"app_id"`
	Service string `json:"service,omitempty"`
}

func (AppUpstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams." + proxy.AppUpstreamSource,
		New: func() caddy.Module { return new(AppUpstreams) },
	}
}

func (u *AppUpstreams) GetUpstreams(*http.Request) ([]*reverseproxy.Upstream, error) {
	appTable.RLock()
	addr := appTable.m[proxy.AppUpstreamKey(u.AppID, u.Service)]
	appTable.RUnlock()
	if addr == "" {
		return nil, nil
	}
	return []*reverseproxy.Upstream{{Dial: addr}}, nil
}

// adminAPI serves GET/POST <admin>/basalt/app-upstreams with the table as
// {"<app id>[/<service>]": "ip:port"}. POST replaces the whole table.
type adminAPI struct{}

func (adminAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "admin.api.basalt", New: func() caddy.Module { return new(adminAPI) }}
}

func (adminAPI) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{{Pattern: proxy.AppUpstreamsPath, Handler: caddy.AdminHandlerFunc(handleAppUpstreams)}}
}

func handleAppUpstreams(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet:
		appTable.RLock()
		defer appTable.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(appTable.m)
	case http.MethodPost:
		var in map[string]string
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
			return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
		}
		m, err := decodeTable(in)
		if err != nil {
			return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
		}
		appTable.Lock()
		defer appTable.Unlock()
		appTable.m = m
		if appTable.file != "" {
			if err := writeTable(appTable.file, m); err != nil {
				return caddy.APIError{HTTPStatus: http.StatusInternalServerError, Err: err}
			}
		}
		return nil
	default:
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method %s not allowed", r.Method)}
	}
}

// decodeTable validates the table: app upstream keys and IP:port addresses.
func decodeTable(in map[string]string) (map[string]string, error) {
	m := make(map[string]string, len(in))
	for k, v := range in {
		if _, _, err := proxy.ParseAppUpstreamKey(k); err != nil {
			return nil, err
		}
		if _, err := netip.ParseAddrPort(v); err != nil {
			return nil, fmt.Errorf("app %s: upstream must be ip:port", k)
		}
		m[k] = v
	}
	return m, nil
}

func writeTable(path string, m map[string]string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadTable restores the table saved by an earlier run. A missing or broken
// file starts empty: the agent pushes the table again.
func loadTable(dir string) error {
	path := filepath.Join(dir, "app-upstreams.json")
	appTable.Lock()
	defer appTable.Unlock()
	appTable.file = path
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var in map[string]string
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	m, err := decodeTable(in)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	appTable.m = maps.Clone(m)
	return nil
}

var (
	_ reverseproxy.UpstreamSource = (*AppUpstreams)(nil)
	_ caddy.AdminRouter           = (*adminAPI)(nil)
)
