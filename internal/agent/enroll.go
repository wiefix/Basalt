package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
)

type EnrollOptions struct {
	DataDir  string
	PanelURL string // e.g. https://panel.example.com:8443
	Token    string
	CASHA256 string // pinned fingerprint of the panel's agent CA
}

// Enroll exchanges a one-time token for a client certificate. The private key
// is generated here and never leaves the host.
func Enroll(ctx context.Context, o EnrollOptions) (int64, error) {
	// A server the panel removed may enroll again in place: its identity
	// is dead, while its other data (SFTP host key, DNS zones) is kept.
	if _, err := os.Stat(filepath.Join(o.DataDir, fileRemoved)); enrolled(o.DataDir) && err != nil {
		return 0, fmt.Errorf("already enrolled (remove %s to re-enroll)", o.DataDir)
	}
	base, err := parsePanelURL(o.PanelURL)
	if err != nil {
		return 0, err
	}
	if o.Token == "" || o.CASHA256 == "" {
		return 0, errors.New("token and CA fingerprint are required")
	}

	key, err := pki.NewKey()
	if err != nil {
		return 0, err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(agentproto.EnrollRequest{Token: o.Token, CSR: csr})

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				// System roots are not used; VerifyPeerCertificate checks the
				// chain against the pinned CA instead.
				InsecureSkipVerify:    true,
				VerifyPeerCertificate: pki.VerifyPinnedPanel(o.CASHA256, time.Now),
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+agentproto.PathEnroll, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("contact panel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("panel refused enrollment (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var er agentproto.EnrollResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&er); err != nil {
		return 0, fmt.Errorf("decode enrollment response: %w", err)
	}
	if pki.Fingerprint(er.CACert) != strings.ToLower(strings.ReplaceAll(o.CASHA256, ":", "")) {
		return 0, errors.New("panel returned a CA that does not match the pinned fingerprint")
	}
	if err := saveEnrollment(o.DataDir, config{PanelURL: base, ServerID: er.ServerID}, key, er.Cert, er.CACert); err != nil {
		return 0, fmt.Errorf("save credentials: %w", err)
	}
	// Validate what we stored the same way Run will load it.
	if _, err := loadIdentity(o.DataDir); err != nil {
		return 0, fmt.Errorf("stored credentials are invalid: %w", err)
	}
	return er.ServerID, nil
}

func parsePanelURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", fmt.Errorf("panel URL must look like https://host:port, got %q", s)
	}
	return u.Scheme + "://" + u.Host, nil
}
