package pki

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
	"time"
)

var now = time.Unix(1_800_000_000, 0)

func TestCARoundTrip(t *testing.T) {
	ca, err := NewCA("test", now)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ca.MarshalKey()
	ca2, err := LoadCA(ca.Cert.Raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if ca2.Fingerprint() != ca.Fingerprint() {
		t.Fatal("fingerprint changed after reload")
	}

	other, _ := NewCA("other", now)
	otherKey, _ := other.MarshalKey()
	if _, err := LoadCA(ca.Cert.Raw, otherKey); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

func TestSignAgentAndVerify(t *testing.T) {
	ca, _ := NewCA("test", now)
	key, _ := NewKey()
	csr, _ := NewCSR(key)
	der, err := ca.SignAgent(csr, 42, now)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)

	_, err = cert.Verify(x509.VerifyOptions{
		Roots: ca.Pool(), CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id, err := AgentID(cert); err != nil || id != 42 {
		t.Fatalf("AgentID = %d, %v", id, err)
	}
	// An agent certificate must not be usable as a server certificate.
	_, err = cert.Verify(x509.VerifyOptions{
		Roots: ca.Pool(), CurrentTime: now, DNSName: PanelServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err == nil {
		t.Fatal("agent certificate accepted for server auth")
	}
	if !cert.NotAfter.After(now.Add(AgentValidity - time.Hour)) {
		t.Fatal("unexpected validity")
	}
}

func TestSignAgentRejectsBadCSR(t *testing.T) {
	ca, _ := NewCA("test", now)
	if _, err := ca.SignAgent([]byte("garbage"), 1, now); err == nil {
		t.Fatal("garbage CSR accepted")
	}
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}, edKey)
	if _, err := ca.SignAgent(csr, 1, now); err == nil {
		t.Fatal("unsupported key type accepted")
	}
	// Tampered CSR: signature no longer matches.
	key, _ := NewKey()
	good, _ := NewCSR(key)
	bad := append([]byte(nil), good...)
	bad[len(bad)-5] ^= 0xff
	if _, err := ca.SignAgent(bad, 1, now); err == nil {
		t.Fatal("tampered CSR accepted")
	}
}

func TestPanelCertificatesAndPinning(t *testing.T) {
	ca, _ := NewCA("test", now)
	clock := now
	pc := ca.PanelCertificates(func() time.Time { return clock })
	c1, err := pc.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2, _ := pc.GetCertificate(nil); c2 != c1 {
		t.Fatal("certificate not cached")
	}

	verify := VerifyPinnedPanel(ca.Fingerprint(), func() time.Time { return clock })
	if err := verify(c1.Certificate, nil); err != nil {
		t.Fatalf("pinned verify: %v", err)
	}

	other, _ := NewCA("evil", now)
	if err := VerifyPinnedPanel(other.Fingerprint(), func() time.Time { return clock })(c1.Certificate, nil); err == nil {
		t.Fatal("wrong pin accepted")
	}
	// An attacker presenting our CA cert next to their own leaf must fail.
	evilLeaf, _ := other.PanelCertificates(func() time.Time { return clock }).GetCertificate(nil)
	if err := verify([][]byte{evilLeaf.Certificate[0], ca.Cert.Raw}, nil); err == nil {
		t.Fatal("foreign leaf accepted with pinned CA attached")
	}

	clock = clock.Add(panelValidity)
	c3, _ := pc.GetCertificate(nil)
	if c3 == c1 {
		t.Fatal("certificate not renewed near expiry")
	}
	if _, ok := c3.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatal("unexpected key type")
	}
}
