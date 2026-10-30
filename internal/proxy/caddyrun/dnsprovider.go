package caddyrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/libdns/libdns"

	"github.com/wiefix/basalt/internal/proxy"
)

func init() {
	caddy.RegisterModule(DNSProvider{})
}

// ChallengeRequest is what the proxy sends the agent on its socket.
type ChallengeRequest struct {
	Name  string `json:"name"` // absolute, without the trailing dot
	Value string `json:"value"`
}

// ChallengePath is the agent's endpoint: POST sets a record, DELETE
// removes it.
const ChallengePath = "/acme-challenge"

// DNSProvider sets DNS-01 challenge records (TXT _acme-challenge.<name>)
// by asking the local agent, which has the panel put them into the zone on
// every DNS server. The proxy holds no credentials of its own.
type DNSProvider struct {
	Socket string `json:"socket,omitempty"`
}

func (DNSProvider) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "dns.providers." + proxy.DNSProviderModule,
		New: func() caddy.Module { return new(DNSProvider) },
	}
}

func (p *DNSProvider) client() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p.Socket)
		},
	}}
}

func (p *DNSProvider) send(ctx context.Context, method, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if p.Socket == "" {
		return nil, errors.New("no agent socket configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, rec := range recs {
		rr := rec.RR()
		if rr.Type != "TXT" {
			return nil, fmt.Errorf("only TXT records are supported, not %s", rr.Type)
		}
		body, _ := json.Marshal(ChallengeRequest{
			Name:  strings.TrimSuffix(libdns.AbsoluteName(rr.Name, zone), "."),
			Value: rr.Data,
		})
		req, err := http.NewRequestWithContext(ctx, method, "http://agent"+ChallengePath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("ask the agent: %w", err)
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return nil, fmt.Errorf("agent: %s", strings.TrimSpace(string(msg)))
		}
	}
	return recs, nil
}

// AppendRecords implements libdns.RecordAppender.
func (p *DNSProvider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.send(ctx, http.MethodPost, zone, recs)
}

// DeleteRecords implements libdns.RecordDeleter.
func (p *DNSProvider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.send(ctx, http.MethodDelete, zone, recs)
}

var (
	_ libdns.RecordAppender = (*DNSProvider)(nil)
	_ libdns.RecordDeleter  = (*DNSProvider)(nil)
)
