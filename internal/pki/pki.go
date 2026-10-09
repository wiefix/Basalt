// Package pki runs the private certificate authority that authenticates the
// control plane and agents to each other (mutual TLS).
//
// Agents never trust the system root store for the panel: they pin the
// fingerprint of this CA, delivered in the enrollment command, and require the
// panel certificate to carry the fixed name PanelServerName. The address an
// agent dials therefore does not need to match any certificate name.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	PanelServerName = "basalt-panel"
	agentURIPrefix  = "basalt://server/"

	caValidity     = 20 * 365 * 24 * time.Hour
	AgentValidity  = 90 * 24 * time.Hour
	panelValidity  = 30 * 24 * time.Hour
	clockSkewSlack = 5 * time.Minute
)

type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func NewCA(commonName string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkewSlack),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, key: key}, nil
}

// LoadCA restores a CA from its certificate and PKCS#8 private key (DER).
func LoadCA(certDER, keyDER []byte) (*CA, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("CA key does not match certificate")
	}
	return &CA{Cert: cert, key: key}, nil
}

func (ca *CA) MarshalKey() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(ca.key) }

func (ca *CA) Fingerprint() string { return Fingerprint(ca.Cert.Raw) }

func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// Fingerprint is the hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// SignAgent issues a client certificate for server id from a CSR. Only the
// CSR's public key is used; identity fields are set by the CA.
func (ca *CA) SignAgent(csrDER []byte, serverID int64, now time.Time) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	if err := checkPublicKey(csr.PublicKey); err != nil {
		return nil, err
	}
	u, _ := url.Parse(agentURIPrefix + strconv.FormatInt(serverID, 10))
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "server-" + strconv.FormatInt(serverID, 10)},
		URIs:         []*url.URL{u},
		NotBefore:    now.Add(-clockSkewSlack),
		NotAfter:     now.Add(AgentValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.key)
}

func checkPublicKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return errors.New("unsupported ECDSA curve")
		}
		return nil
	default:
		return fmt.Errorf("unsupported key type %T (want ECDSA P-256)", pub)
	}
}

// AgentID extracts the server ID from a verified agent certificate.
func AgentID(cert *x509.Certificate) (int64, error) {
	for _, u := range cert.URIs {
		s := u.String()
		if rest, ok := strings.CutPrefix(s, agentURIPrefix); ok {
			id, err := strconv.ParseInt(rest, 10, 64)
			if err != nil || id <= 0 {
				return 0, fmt.Errorf("invalid agent URI %q", s)
			}
			return id, nil
		}
	}
	return 0, errors.New("certificate has no agent identity")
}

// PanelCertificates issues and caches the TLS certificate the panel presents
// to agents, re-issuing it before it expires.
type PanelCertificates struct {
	ca  *CA
	now func() time.Time

	mu   sync.Mutex
	cert *tls.Certificate
}

func (ca *CA) PanelCertificates(now func() time.Time) *PanelCertificates {
	return &PanelCertificates{ca: ca, now: now}
}

func (p *PanelCertificates) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.cert != nil && now.Before(p.cert.Leaf.NotAfter.Add(-panelValidity/3)) {
		return p.cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: PanelServerName},
		DNSNames:     []string{PanelServerName},
		NotBefore:    now.Add(-clockSkewSlack),
		NotAfter:     now.Add(panelValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca.Cert, &key.PublicKey, p.ca.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	p.cert = &tls.Certificate{
		Certificate: [][]byte{der, p.ca.Cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}
	return p.cert, nil
}

// NewKey generates an agent private key.
func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func NewCSR(key crypto.Signer) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "basalt-agent"},
	}, key)
}

// VerifyPinnedPanel returns a tls.Config.VerifyPeerCertificate callback for
// enrollment, before the agent has the CA: the presented chain must contain a
// CA whose fingerprint equals pin, and the leaf must chain to it with the
// panel's fixed name. Use together with InsecureSkipVerify (the callback does
// the verification instead of the system roots).
func VerifyPinnedPanel(pin string, now func() time.Time) func([][]byte, [][]*x509.Certificate) error {
	pin = strings.ToLower(strings.ReplaceAll(pin, ":", ""))
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("panel presented no certificate")
		}
		var root *x509.Certificate
		for _, der := range raw[1:] {
			if subtle.ConstantTimeCompare([]byte(Fingerprint(der)), []byte(pin)) == 1 {
				c, err := x509.ParseCertificate(der)
				if err != nil {
					return err
				}
				root = c
			}
		}
		if root == nil {
			return errors.New("panel CA fingerprint does not match the pinned value")
		}
		leaf, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		roots.AddCert(root)
		_, err = leaf.Verify(x509.VerifyOptions{
			Roots:       roots,
			DNSName:     PanelServerName,
			CurrentTime: now(),
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
}

func randomSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
