package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// CertificatesPath is the admin route of the certificates the proxy holds:
// the agent asks the proxy for them (POST CertificatesRequest →
// CertificatesResponse) rather than reading its storage, which only the
// proxy's user may read.
const CertificatesPath = "/basalt/certificates"

// MaxCertificates bounds the names of one request.
const MaxCertificates = 1000

type CertificatesRequest struct {
	Names []string `json:"names"`
	// Keys: with the private keys (the mail server needs its own).
	Keys bool `json:"keys,omitempty"`
}

// Certificate is the newest certificate for a name, from any issuer (no
// Chain: none yet).
type Certificate struct {
	Name  string `json:"name"`
	Chain []byte `json:"chain,omitempty"` // PEM
	Key   []byte `json:"key,omitempty"`   // PEM, only when asked for
}

type CertificatesResponse struct {
	Certs []Certificate `json:"certs"`
}

// StoredCertificate finds the newest certificate for name (any issuer) in
// the storage of the proxy whose data is in dir.
func StoredCertificate(dir, name string, withKey bool) (Certificate, error) {
	out := Certificate{Name: name}
	if n, err := NormalizeRouteDomain(name); err != nil || n != name {
		return out, errors.New("invalid name")
	}
	// Caddy keeps *.example.com under wildcard_.example.com.
	d := strings.Replace(name, "*", "wildcard_", 1)
	files, _ := filepath.Glob(filepath.Join(storagePath(dir), "certificates", "*", d, d+".crt"))
	var newest *x509.Certificate
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
		if err != nil || newest != nil && !leaf.NotAfter.After(newest.NotAfter) {
			continue
		}
		var k []byte
		if withKey {
			if k, err = os.ReadFile(strings.TrimSuffix(f, ".crt") + ".key"); err != nil {
				continue
			}
		}
		newest, out.Chain, out.Key = leaf, c, k
	}
	return out, nil
}

// Certificates asks the proxy for the newest certificate of each name.
func (a *Admin) Certificates(ctx context.Context, names []string, keys bool) ([]Certificate, error) {
	b, err := json.Marshal(CertificatesRequest{Names: names, Keys: keys})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+CertificatesPath, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("proxy refused the certificates (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out CertificatesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("proxy certificates: %w", err)
	}
	return out.Certs, nil
}
