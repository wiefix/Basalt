package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Admin talks to the proxy's admin API over its unix socket.
type Admin struct {
	hc *http.Client
}

func NewAdmin(socket string) *Admin {
	return &Admin{hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// Load replaces the running configuration atomically: on error the proxy
// keeps serving the previous one.
func (a *Admin) Load(ctx context.Context, cfg []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/load", bytes.NewReader(cfg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("proxy admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("proxy rejected config (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Running reports whether the proxy is serving the config identified by id.
func (a *Admin) Running(ctx context.Context, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/id/"+id, nil)
	if err != nil {
		return false, err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("proxy admin: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK, nil
}
