// Package cloudtest is a fake Hetzner Cloud API for tests: one project
// with a token, its servers and floating IPs.
package cloudtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Server is a server of the fake project.
type Server struct {
	ID   int64
	Name string
	IPv4 string // "" none
	IPv6 string // a /64 block, "" none
}

// FloatingIP is a floating IP of the fake project.
type FloatingIP struct {
	ID       int64
	Name     string
	IP       string // "203.0.113.10" or "2001:db8:1::/64"
	Location string
	Server   int64 // 0: unassigned
}

// Hetzner is the fake API; URL is its base, as the panel names it.
type Hetzner struct {
	*httptest.Server
	URL   string
	Token string

	mu      sync.Mutex
	servers []Server
	ips     []FloatingIP
	// pending actions: id → floating IP and server, done on the next read.
	actions    map[int64][2]int64
	next       int64
	failAssign bool
	assigns    int
}

// FailAssign makes assignments end in an error.
func (h *Hetzner) FailAssign(fail bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failAssign = fail
}

// Assigns counts the assignments asked for.
func (h *Hetzner) Assigns() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.assigns
}

func NewHetzner(t testing.TB, token string) *Hetzner {
	h := &Hetzner{Token: token, actions: map[int64][2]int64{}, next: 1000}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	h.URL = h.Server.URL + "/v1"
	t.Cleanup(h.Close)
	return h
}

func (h *Hetzner) AddServer(s Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.servers = append(h.servers, s)
}

func (h *Hetzner) AddFloatingIP(f FloatingIP) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ips = append(h.ips, f)
}

// AssignedTo is the server a floating IP is routed to (0: none).
func (h *Hetzner) AssignedTo(id int64) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.ips {
		if f.ID == id {
			return f.Server
		}
	}
	return 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func page(page string, n int) map[string]any {
	p, _ := strconv.Atoi(page)
	return map[string]any{"pagination": map[string]any{"page": max(p, 1), "per_page": 50, "next_page": nil, "total_entries": n}}
}

func (h *Hetzner) floatingJSON(f FloatingIP) map[string]any {
	typ := "ipv4"
	if strings.Contains(f.IP, ":") {
		typ = "ipv6"
	}
	var server any
	if f.Server != 0 {
		server = f.Server
	}
	return map[string]any{"id": f.ID, "name": f.Name, "ip": f.IP, "type": typ, "server": server,
		"home_location": map[string]any{"name": f.Location}, "blocked": false}
}

func (h *Hetzner) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.Token {
		apiError(w, http.StatusUnauthorized, "unauthorized", "unable to authenticate")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/servers":
		out := []any{}
		for _, s := range h.servers {
			net := map[string]any{"ipv4": nil, "ipv6": nil}
			if s.IPv4 != "" {
				net["ipv4"] = map[string]any{"ip": s.IPv4}
			}
			if s.IPv6 != "" {
				net["ipv6"] = map[string]any{"ip": s.IPv6}
			}
			out = append(out, map[string]any{"id": s.ID, "name": s.Name, "public_net": net})
		}
		writeJSON(w, http.StatusOK, map[string]any{"servers": out, "meta": page(r.URL.Query().Get("page"), len(out))})
	case r.Method == http.MethodGet && path == "/floating_ips":
		out := []any{}
		for _, f := range h.ips {
			out = append(out, h.floatingJSON(f))
		}
		writeJSON(w, http.StatusOK, map[string]any{"floating_ips": out, "meta": page(r.URL.Query().Get("page"), len(out))})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "floating_ips":
		for _, f := range h.ips {
			if strconv.FormatInt(f.ID, 10) == parts[1] {
				writeJSON(w, http.StatusOK, map[string]any{"floating_ip": h.floatingJSON(f)})
				return
			}
		}
		apiError(w, http.StatusNotFound, "not_found", "floating_ip not found")
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "floating_ips" && parts[3] == "assign":
		var body struct {
			Server int64 `json:"server"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_input", "invalid JSON")
			return
		}
		ipID, _ := strconv.ParseInt(parts[1], 10, 64)
		found := false
		for _, f := range h.ips {
			found = found || f.ID == ipID
		}
		if !found {
			apiError(w, http.StatusNotFound, "not_found", "floating_ip not found")
			return
		}
		known := false
		for _, s := range h.servers {
			known = known || s.ID == body.Server
		}
		if !known {
			apiError(w, http.StatusUnprocessableEntity, "invalid_input", "server not found")
			return
		}
		h.assigns++
		h.next++
		h.actions[h.next] = [2]int64{ipID, body.Server}
		writeJSON(w, http.StatusCreated, map[string]any{"action": map[string]any{
			"id": h.next, "command": "assign_floating_ip", "status": "running", "error": nil}})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "actions":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		a, ok := h.actions[id]
		if !ok {
			apiError(w, http.StatusNotFound, "not_found", "action not found")
			return
		}
		delete(h.actions, id)
		if h.failAssign {
			writeJSON(w, http.StatusOK, map[string]any{"action": map[string]any{"id": id, "status": "error",
				"error": map[string]string{"code": "server_error", "message": "assignment failed"}}})
			return
		}
		for i := range h.ips {
			if h.ips[i].ID == a[0] {
				h.ips[i].Server = a[1]
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"action": map[string]any{"id": id, "status": "success", "error": nil}})
	default:
		apiError(w, http.StatusNotFound, "not_found", "not found")
	}
}
