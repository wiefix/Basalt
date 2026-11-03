// Package dockerproxy serves the Docker Engine API to the agent through the
// privileged helper: access to Docker's own socket is root on the host
// (a privileged container, a bind mount of /, the host's namespaces), so
// the agent gets this proxy instead. It passes only the calls Basalt makes
// (the agent's client, and the docker command line it runs: builds, run,
// exec) and refuses everything that could reach out of a container.
//
// What is checked: every request is matched against a list of endpoints;
// the bodies that describe a container, an exec, a network or a volume are
// read field by field, and a field the proxy does not know must be empty —
// a new Docker option is refused until it is looked at. Exec and attach
// are refused for containers that are not confined like Basalt's (made
// outside the proxy), except "buildctl dial-stdio" in the helper's own
// BuildKit container, which is how builds reach it.
package dockerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// maxBody bounds a request body the proxy reads to check it.
const maxBody = 1 << 20

// Proxy is an http.Handler forwarding allowed requests to Docker.
type Proxy struct {
	upstream string
	// buildKitImage is the image of the helper's BuildKit container ("":
	// none), the one privileged container the agent may exec into.
	buildKitImage string
	log           *slog.Logger
	rp            *httputil.ReverseProxy
	hc            *http.Client
}

// New returns a proxy to the Docker daemon listening on upstream.
func New(upstream, buildKitImage string, log *slog.Logger) *Proxy {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", upstream)
		},
		MaxIdleConns:    8,
		IdleConnTimeout: time.Minute,
	}
	p := &Proxy{upstream: upstream, buildKitImage: buildKitImage, log: log,
		// No timeout: inspecting a container is one short call, bounded
		// by the request's context.
		hc: &http.Client{Transport: tr}}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host = "http", "docker"
			r.Out.Host = "docker"
		},
		Transport: tr,
		// Logs, pulls and builds stream; attach, exec and BuildKit's
		// sessions switch protocols, which ReverseProxy passes through.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				log.Warn("docker request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			}
			writeError(w, http.StatusBadGateway, "the Docker daemon did not answer")
		},
	}
	return p
}

// errDenied wraps the reason a request is refused.
type errDenied struct{ reason string }

func (e errDenied) Error() string { return e.reason }

func deny(format string, args ...any) error { return errDenied{fmt.Sprintf(format, args...)} }

// versionPrefix is the optional "/v1.44" of a path.
var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+/`)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(p.upstream); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusServiceUnavailable, "Docker is not installed on this server")
		return
	}
	path := r.URL.Path
	if loc := versionPrefix.FindStringIndex(path); loc != nil {
		path = path[loc[1]-1:]
	}
	err := p.check(r, path)
	if err != nil {
		var d errDenied
		if errors.As(err, &d) {
			p.log.Warn("refused a Docker request", "method", r.Method, "path", r.URL.Path, "reason", d.reason)
			writeError(w, http.StatusForbidden, "refused by Basalt: "+d.reason)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.rp.ServeHTTP(w, r)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// check decides on a request (path without the API version).
func (p *Proxy) check(r *http.Request, path string) error {
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// Names of containers, images, networks and volumes need no escaping:
	// a path the daemon might read otherwise than the proxy is refused.
	if r.URL.RawPath != "" || slices.Contains(seg, ".") || slices.Contains(seg, "..") {
		return deny("unusual path %q", r.URL.EscapedPath())
	}
	m := r.Method
	get := m == http.MethodGet || m == http.MethodHead
	switch {
	case len(seg) == 1 && seg[0] == "_ping" && get,
		len(seg) == 1 && (seg[0] == "version" || seg[0] == "info") && m == http.MethodGet:
		return nil

	case seg[0] == "containers":
		return p.checkContainers(r, seg[1:])
	case seg[0] == "exec" && len(seg) == 3 && seg[1] != "":
		// Execs exist only as the proxy let them be created.
		switch {
		case seg[2] == "start" && m == http.MethodPost, seg[2] == "resize" && m == http.MethodPost,
			seg[2] == "json" && m == http.MethodGet:
			return nil
		}
	case seg[0] == "images":
		return checkImages(r, seg[1:])
	case seg[0] == "build":
		switch {
		case len(seg) == 1 && m == http.MethodPost:
			if nm := r.URL.Query().Get("networkmode"); nm == "host" || strings.HasPrefix(nm, "container:") {
				return deny("builds may not use the network %q", nm)
			}
			return nil
		case len(seg) == 2 && seg[1] == "prune" && m == http.MethodPost:
			return nil
		}
	case len(seg) == 1 && (seg[0] == "session" || seg[0] == "grpc") && m == http.MethodPost:
		// BuildKit's own protocol (builds of the docker command).
		return nil
	case seg[0] == "networks":
		return checkNetworks(r, seg[1:])
	case seg[0] == "volumes":
		return checkVolumes(r, seg[1:])
	}
	return deny("%s %s is not used by Basalt", m, path)
}

func (p *Proxy) checkContainers(r *http.Request, seg []string) error {
	m := r.Method
	switch {
	case len(seg) == 1 && seg[0] == "json" && m == http.MethodGet:
		return nil
	case len(seg) == 1 && seg[0] == "create" && m == http.MethodPost:
		return checkBody(r, checkContainerCreate)
	case len(seg) == 1 && seg[0] != "" && m == http.MethodDelete:
		return nil
	case len(seg) != 2 || seg[0] == "":
		return deny("%s /containers/%s is not used by Basalt", m, strings.Join(seg, "/"))
	}
	id, op := seg[0], seg[1]
	switch {
	case m == http.MethodGet && (op == "json" || op == "logs" || op == "stats"),
		m == http.MethodPost && (op == "start" || op == "stop" || op == "kill" || op == "restart" || op == "wait" || op == "resize"):
		return nil
	case m == http.MethodPost && op == "attach":
		c, err := p.inspect(r.Context(), id)
		if err != nil {
			return err
		}
		if c != nil {
			if reason := c.unconfined(); reason != "" {
				return deny("cannot attach to a container that %s", reason)
			}
		}
		return nil
	case m == http.MethodPost && op == "exec":
		c, err := p.inspect(r.Context(), id)
		if err != nil {
			return err
		}
		var exec map[string]json.RawMessage
		if err := checkBody(r, func(o map[string]json.RawMessage) error { exec = o; return checkExec(o) }); err != nil {
			return err
		}
		if c == nil {
			return nil
		}
		if reason := c.unconfined(); reason != "" {
			if p.buildKitImage == "" || c.Config.Image != p.buildKitImage || !buildKitDial(exec) {
				return deny("cannot run commands in a container that %s", reason)
			}
		}
		return nil
	}
	return deny("%s /containers/{id}/%s is not used by Basalt", m, op)
}

// buildKitDial reports whether an exec is BuildKit's "buildctl dial-stdio"
// (buildx's remote driver connecting) and nothing more.
func buildKitDial(o map[string]json.RawMessage) bool {
	var cmd []string
	if json.Unmarshal(field(o, "Cmd"), &cmd) != nil || len(cmd) != 2 || cmd[0] != "buildctl" || cmd[1] != "dial-stdio" {
		return false
	}
	for _, k := range []string{"User", "Env", "WorkingDir"} {
		if !isZero(field(o, k)) {
			return false
		}
	}
	return true
}

func checkImages(r *http.Request, seg []string) error {
	m, q := r.Method, r.URL.Query()
	switch {
	case len(seg) == 1 && seg[0] == "create" && m == http.MethodPost:
		// Only pulls: an import (fromSrc) would have the daemon fetch any URL.
		if q.Get("fromImage") == "" || q.Has("fromSrc") {
			return deny("images are only pulled")
		}
		return nil
	case len(seg) == 1 && seg[0] == "load" && m == http.MethodPost,
		len(seg) == 1 && seg[0] == "json" && m == http.MethodGet:
		return nil
	case len(seg) >= 1 && seg[0] != "" && m == http.MethodDelete:
		return nil
	case len(seg) >= 2 && seg[0] != "":
		switch op := seg[len(seg)-1]; {
		case op == "json" && m == http.MethodGet, (op == "push" || op == "tag") && m == http.MethodPost:
			return nil
		}
	}
	return deny("%s /images/%s is not used by Basalt", m, strings.Join(seg, "/"))
}

func checkNetworks(r *http.Request, seg []string) error {
	m := r.Method
	switch {
	case len(seg) == 0 && m == http.MethodGet:
		return nil
	case len(seg) == 1 && seg[0] == "create" && m == http.MethodPost:
		return checkBody(r, checkNetworkCreate)
	case len(seg) == 1 && seg[0] != "" && (m == http.MethodGet || m == http.MethodDelete):
		if seg[0] == "host" && m == http.MethodDelete {
			return deny("the host network stays")
		}
		return nil
	case len(seg) == 2 && seg[0] != "" && m == http.MethodPost && (seg[1] == "connect" || seg[1] == "disconnect"):
		if seg[0] == "host" {
			return deny("containers may not join the host network")
		}
		if seg[1] == "connect" {
			return checkBody(r, checkNetworkConnect)
		}
		return nil
	}
	return deny("%s /networks/%s is not used by Basalt", m, strings.Join(seg, "/"))
}

func checkVolumes(r *http.Request, seg []string) error {
	m := r.Method
	switch {
	case len(seg) == 0 && m == http.MethodGet:
		return nil
	case len(seg) == 1 && seg[0] == "create" && m == http.MethodPost:
		return checkBody(r, checkVolumeCreate)
	case len(seg) == 1 && seg[0] != "" && (m == http.MethodGet || m == http.MethodDelete):
		return nil
	}
	return deny("%s /volumes/%s is not used by Basalt", m, strings.Join(seg, "/"))
}

// checkBody reads the request's JSON object, checks it and puts it back
// for the daemon, byte for byte.
func checkBody(r *http.Request, check func(map[string]json.RawMessage) error) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body.Close()
	if err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	if len(b) > maxBody {
		return deny("the request is too large")
	}
	o := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &o); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
	}
	if err := check(o); err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", fmt.Sprint(len(b)))
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
	return nil
}

// container is what the proxy needs to know of an existing container.
type container struct {
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	HostConfig struct {
		Privileged     bool              `json:"Privileged"`
		NetworkMode    string            `json:"NetworkMode"`
		PidMode        string            `json:"PidMode"`
		IpcMode        string            `json:"IpcMode"`
		UTSMode        string            `json:"UTSMode"`
		UsernsMode     string            `json:"UsernsMode"`
		CgroupnsMode   string            `json:"CgroupnsMode"`
		Binds          []string          `json:"Binds"`
		Mounts         []json.RawMessage `json:"Mounts"`
		Devices        []json.RawMessage `json:"Devices"`
		DeviceRequests []json.RawMessage `json:"DeviceRequests"`
		CapAdd         []string          `json:"CapAdd"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		MaskedPaths    *[]string         `json:"MaskedPaths"`
	} `json:"HostConfig"`
}

// unconfined says how a container (made by anyone: inspected settings
// carry the daemon's defaults, so this is not checkHostConfig) reaches out
// of its confinement, "" if it does not.
func (c *container) unconfined() string {
	h := c.HostConfig
	switch {
	case h.Privileged:
		return "is privileged"
	case h.NetworkMode == "host", h.PidMode == "host", h.IpcMode == "host", h.UTSMode == "host",
		h.UsernsMode == "host", h.CgroupnsMode == "host":
		return "shares the host's namespaces"
	case strings.HasPrefix(h.NetworkMode, "container:"), strings.HasPrefix(h.PidMode, "container:"),
		strings.HasPrefix(h.IpcMode, "container:"):
		return "shares another container's namespaces"
	case len(h.Binds) > 0:
		return "mounts host paths"
	case len(h.Devices) > 0, len(h.DeviceRequests) > 0:
		return "has host devices"
	case len(h.CapAdd) > 0:
		return "has added capabilities"
	case h.MaskedPaths != nil && len(*h.MaskedPaths) == 0:
		return "sees all of /proc"
	}
	for _, m := range h.Mounts {
		if checkMount(m) != nil {
			return "mounts host paths"
		}
	}
	for _, o := range h.SecurityOpt {
		if checkSecurityOpt(o) != nil {
			return "runs without the usual security profile"
		}
	}
	return ""
}

// inspect returns a container, nil if there is none (the daemon then
// answers the request itself: not found).
func (p *Proxy) inspect(ctx context.Context, id string) (*container, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("inspect the container: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = resp.Status
		}
		return nil, fmt.Errorf("inspect the container: %s", e.Message)
	}
	var c container
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&c); err != nil {
		return nil, fmt.Errorf("inspect the container: %w", err)
	}
	return &c, nil
}
