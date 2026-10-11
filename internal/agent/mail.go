package agent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/mailserver"
)

// Bounds of a relayed mail management call (the channel's frames are at
// most 4 MiB).
const (
	maxMailRequest  = 1 << 20
	maxMailResponse = 3 << 20
)

// MailAPI posts a JMAP management request to the app's mail server (its
// published HTTP port on the app network) and returns the answer.
func (m *appsManager) MailAPI(ctx context.Context, req agentproto.MailAPI) (agentproto.MailAPIResponse, error) {
	if len(req.Body) == 0 || len(req.Body) > maxMailRequest || req.User == "" {
		return agentproto.MailAPIResponse{}, errors.New("invalid mail API request")
	}
	status, body, err := m.mailPost(ctx, req.AppID, req.User, req.Password, req.Body)
	return agentproto.MailAPIResponse{Status: status, Body: body}, err
}

func (m *appsManager) mailPost(ctx context.Context, appID int64, user, password string, body []byte) (int, []byte, error) {
	dc, err := m.client()
	if err != nil {
		return 0, nil, err
	}
	c, err := m.current(ctx, dc, appID, "")
	if err != nil {
		return 0, nil, err
	}
	if c == nil || c.State != "running" {
		return 0, nil, errors.New("the mail server is not running")
	}
	port := int(labelInt(c.Labels, labelPort))
	info, err := dc.ContainerInspect(ctx, c.ID)
	if err != nil {
		return 0, nil, err
	}
	addr := m.upstreamOf(info, appID, port)
	if port <= 0 || addr == "" {
		return 0, nil, errors.New("the mail server has no management address")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/jmap/", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.SetBasicAuth(user, password)
	resp, err := (&http.Client{}).Do(hr)
	if err != nil {
		return 0, nil, fmt.Errorf("mail server: %w", publicError(err))
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxMailResponse+1))
	if err != nil {
		return 0, nil, err
	}
	if len(out) > maxMailResponse {
		return 0, nil, errors.New("mail server answer too large")
	}
	return resp.StatusCode, out, nil
}

// MailCheck runs the deliverability checks of this host (one at a time).
func (a *Agent) MailCheck(ctx context.Context, req agentproto.MailCheck) (agentproto.MailCheckResponse, error) {
	if mailserver.ValidHostname(req.Hostname) != nil || len(req.IPs) > 16 || len(req.Domains) > 1000 {
		return agentproto.MailCheckResponse{}, errors.New("invalid mail check request")
	}
	if !a.mailCheckMu.TryLock() {
		return agentproto.MailCheckResponse{}, errors.New("a mail check is already running")
	}
	defer a.mailCheckMu.Unlock()
	return agentproto.MailCheckResponse{Results: a.mailCheck.Run(ctx, req)}, nil
}

// MailCert loads the proxy's certificate for the mail host name into the
// mail server, so IMAP and SMTP use the same certificate as HTTPS. The
// key never leaves the host.
func (a *Agent) MailCert(ctx context.Context, req agentproto.MailCert) (agentproto.MailCertResponse, error) {
	var out agentproto.MailCertResponse
	chain, key, notAfter, err := proxyCertificate(a.proxy.opts.Dir, req.Hostname)
	if err != nil {
		return out, err
	}
	c := mailserver.NewClient(func(ctx context.Context, body []byte) (int, []byte, error) {
		return a.apps.mailPost(ctx, req.AppID, req.User, req.Password, body)
	})
	out.NotAfter = notAfter
	out.Installed, err = c.InstallCertificate(ctx, chain, key)
	return out, err
}

// proxyCertificate finds the newest certificate the proxy holds for host
// (any issuer) in its storage.
func proxyCertificate(proxyDir, host string) (chain, key []byte, notAfter time.Time, err error) {
	if mailserver.ValidHostname(host) != nil {
		return nil, nil, notAfter, errors.New("invalid host name")
	}
	files, _ := filepath.Glob(filepath.Join(proxyDir, "storage", "certificates", "*", host, host+".crt"))
	for _, f := range files {
		c, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		block, _ := pem.Decode(c)
		if block == nil {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !leaf.NotAfter.After(notAfter) {
			continue
		}
		k, err := os.ReadFile(strings.TrimSuffix(f, ".crt") + ".key")
		if err != nil {
			continue
		}
		chain, key, notAfter = c, k, leaf.NotAfter
	}
	if chain == nil {
		return nil, nil, notAfter, errors.New("the proxy has no certificate for " + host + " yet")
	}
	return chain, key, notAfter, nil
}
