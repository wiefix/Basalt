package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// writeCert stores a self-signed certificate where Caddy keeps the
// certificate of name issued by issuerDir.
func writeCert(t *testing.T, proxyDir, issuerDir, name, issuer string, notAfter time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: issuer}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proxyDir, "storage", "certificates", issuerDir, name)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
}

func TestCertStatus(t *testing.T) {
	dir := t.TempDir()
	soon, later := time.Now().Add(10*24*time.Hour).Truncate(time.Second), time.Now().Add(80*24*time.Hour).Truncate(time.Second)
	writeCert(t, dir, "acme-v02.api.letsencrypt.org-directory", "shop.example.com", "R11", soon)
	writeCert(t, dir, "acme.zerossl.com-v2-dv90", "shop.example.com", "ZeroSSL", later) // newest wins
	writeCert(t, dir, "acme-v02.api.letsencrypt.org-directory", "wildcard_.example.org", "E6", soon)
	resp, err := certStatus(dir, agentproto.CertStatus{Names: []string{"shop.example.com", "*.example.org", "new.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Certs
	if len(c) != 3 || !c[0].NotAfter.Equal(later) || c[0].Issuer != "ZeroSSL" || !c[1].NotAfter.Equal(soon) || c[1].Issuer != "E6" ||
		!c[2].NotAfter.IsZero() {
		t.Fatalf("certs = %+v", c)
	}
	for _, bad := range []string{"../etc", "Shop.example.com", "a/b.example.com", "*"} {
		if _, err := certStatus(dir, agentproto.CertStatus{Names: []string{bad}}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
