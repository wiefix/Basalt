package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HetznerAPI is the Hetzner Cloud API.
const HetznerAPI = "https://api.hetzner.cloud/v1"

type hetzner struct {
	api   string
	token string
	http  *http.Client
	// poll is how often Assign asks whether the provider is done.
	poll time.Duration
}

func newHetzner(token, api string) *hetzner {
	if api == "" {
		api = HetznerAPI
	}
	return &hetzner{api: strings.TrimSuffix(api, "/"), token: token, poll: time.Second,
		http: &http.Client{Timeout: 30 * time.Second}}
}

// maxPages bounds a listing (50 entries a page).
const maxPages = 40

func (h *hetzner) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		// The token is in a header, never in the URL the error names.
		return fmt.Errorf("provider API: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("provider API: %w", err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if resp.StatusCode == http.StatusNotFound && e.Error.Code == "not_found" {
			return ErrNotFound
		}
		return &Error{Status: resp.StatusCode, Code: e.Error.Code, Message: truncate(e.Error.Message, 300)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("provider API: unexpected answer: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type hetznerPage struct {
	Meta struct {
		Pagination struct {
			NextPage *int `json:"next_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

// list reads every page of a listing; each page's body goes to add.
func (h *hetzner) list(ctx context.Context, path string, add func([]byte) error) error {
	page := 1
	for range maxPages {
		var raw json.RawMessage
		if err := h.do(ctx, http.MethodGet, path+"?per_page=50&page="+strconv.Itoa(page), nil, &raw); err != nil {
			return err
		}
		if err := add(raw); err != nil {
			return fmt.Errorf("provider API: unexpected answer: %w", err)
		}
		var p hetznerPage
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("provider API: unexpected answer: %w", err)
		}
		if p.Meta.Pagination.NextPage == nil || *p.Meta.Pagination.NextPage <= page {
			return nil
		}
		page = *p.Meta.Pagination.NextPage
	}
	return errors.New("provider API: too many pages")
}

type hetznerFloatingIP struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	IP           string `json:"ip"`
	Type         string `json:"type"`
	Server       *int64 `json:"server"`
	HomeLocation struct {
		Name string `json:"name"`
	} `json:"home_location"`
}

func (f hetznerFloatingIP) convert() (FloatingIP, error) {
	out := FloatingIP{ID: strconv.FormatInt(f.ID, 10), Name: f.Name, Network: f.IP, Location: f.HomeLocation.Name}
	if f.Server != nil {
		out.ServerID = strconv.FormatInt(*f.Server, 10)
	}
	addr, err := hostAddress(f.IP)
	if err != nil {
		return out, err
	}
	out.Address = addr.String()
	return out, nil
}

// hostAddress is the address a server configures for a floating IP: the
// address itself, or the first one of an IPv6 block (as the provider's
// documentation does).
func hostAddress(ip string) (netip.Addr, error) {
	if p, err := netip.ParsePrefix(ip); err == nil {
		if !p.Addr().Is6() || p.Bits() > 64 {
			return netip.Addr{}, fmt.Errorf("unexpected floating IP %q", ip)
		}
		return p.Masked().Addr().Next(), nil
	}
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return netip.Addr{}, fmt.Errorf("unexpected floating IP %q", ip)
	}
	return a, nil
}

func (h *hetzner) FloatingIPs(ctx context.Context) ([]FloatingIP, error) {
	var out []FloatingIP
	err := h.list(ctx, "/floating_ips", func(b []byte) error {
		var page struct {
			FloatingIPs []hetznerFloatingIP `json:"floating_ips"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		for _, f := range page.FloatingIPs {
			if ip, err := f.convert(); err == nil {
				out = append(out, ip)
			}
		}
		return nil
	})
	return out, err
}

func validID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0
}

func (h *hetzner) FloatingIP(ctx context.Context, id string) (*FloatingIP, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	var resp struct {
		FloatingIP hetznerFloatingIP `json:"floating_ip"`
	}
	if err := h.do(ctx, http.MethodGet, "/floating_ips/"+id, nil, &resp); err != nil {
		return nil, err
	}
	ip, err := resp.FloatingIP.convert()
	if err != nil {
		return nil, err
	}
	return &ip, nil
}

func (h *hetzner) Servers(ctx context.Context) ([]Server, error) {
	var out []Server
	err := h.list(ctx, "/servers", func(b []byte) error {
		var page struct {
			Servers []struct {
				ID        int64  `json:"id"`
				Name      string `json:"name"`
				PublicNet struct {
					IPv4 *struct {
						IP string `json:"ip"`
					} `json:"ipv4"`
					IPv6 *struct {
						IP string `json:"ip"`
					} `json:"ipv6"`
				} `json:"public_net"`
			} `json:"servers"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		for _, s := range page.Servers {
			srv := Server{ID: strconv.FormatInt(s.ID, 10), Name: s.Name}
			if s.PublicNet.IPv4 != nil {
				srv.IPv4 = s.PublicNet.IPv4.IP
			}
			if s.PublicNet.IPv6 != nil {
				srv.IPv6 = s.PublicNet.IPv6.IP
			}
			out = append(out, srv)
		}
		return nil
	})
	return out, err
}

type hetznerAction struct {
	Action struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"action"`
}

func (h *hetzner) Assign(ctx context.Context, ipID, serverID string) error {
	if !validID(ipID) {
		return ErrNotFound
	}
	sid, err := strconv.ParseInt(serverID, 10, 64)
	if err != nil || sid <= 0 {
		return fmt.Errorf("invalid provider server id %q", serverID)
	}
	var act hetznerAction
	if err := h.do(ctx, http.MethodPost, "/floating_ips/"+ipID+"/actions/assign", map[string]int64{"server": sid}, &act); err != nil {
		return err
	}
	for {
		switch act.Action.Status {
		case "success":
			return nil
		case "error":
			if e := act.Action.Error; e != nil {
				return &Error{Status: http.StatusOK, Code: e.Code, Message: truncate(e.Message, 300)}
			}
			return errors.New("provider API: assigning the floating IP failed")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("provider API: assigning the floating IP: %w", ctx.Err())
		case <-time.After(h.poll):
		}
		id := strconv.FormatInt(act.Action.ID, 10)
		if err := h.do(ctx, http.MethodGet, "/actions/"+url.PathEscape(id), nil, &act); err != nil {
			return err
		}
	}
}
