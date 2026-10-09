// Package agent is the per-host daemon. It enrolls with the control plane
// once, then keeps a mutual-TLS stream open to it, reports host facts and
// metrics, and serves requests from the panel.
package agent

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	fileConfig = "agent.json"
	// Key and certificate share one file so renewal replaces both in a
	// single atomic rename.
	fileCreds = "agent.pem"
	fileCA    = "ca.crt"
)

var ErrNotEnrolled = errors.New("agent is not enrolled; run `basalt agent enroll` first")

type config struct {
	PanelURL string `json:"panel_url"`
	ServerID int64  `json:"server_id"`
}

// identity is everything the agent needs to authenticate to the panel.
type identity struct {
	config
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	ca   *x509.Certificate
}

func (id *identity) tlsCertificate() tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{id.cert.Raw}, PrivateKey: id.key, Leaf: id.cert}
}

func enrolled(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, fileConfig))
	return err == nil
}

func loadIdentity(dir string) (*identity, error) {
	b, err := os.ReadFile(filepath.Join(dir, fileConfig))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotEnrolled
	}
	if err != nil {
		return nil, err
	}
	var id identity
	if err := json.Unmarshal(b, &id.config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", fileConfig, err)
	}

	credsPath := filepath.Join(dir, fileCreds)
	if info, err := os.Stat(credsPath); err != nil {
		return nil, err
	} else if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %v; must be 0600", credsPath, info.Mode().Perm())
	}
	if id.key, id.cert, err = readCreds(credsPath); err != nil {
		return nil, err
	}
	if id.ca, err = readCert(filepath.Join(dir, fileCA)); err != nil {
		return nil, err
	}
	if !id.key.PublicKey.Equal(id.cert.PublicKey) {
		return nil, errors.New("agent key does not match agent certificate")
	}
	return &id, nil
}

// saveCredentials atomically replaces the key and certificate.
func saveCredentials(dir string, key *ecdsa.PrivateKey, certDER []byte) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	data := append(pemBytes("PRIVATE KEY", keyDER), pemBytes("CERTIFICATE", certDER)...)
	return writeAtomic(filepath.Join(dir, fileCreds), data, 0o600)
}

func readCreds(path string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var key *ecdsa.PrivateKey
	var cert *x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return nil, nil, fmt.Errorf("parse agent key: %w", err)
			}
			var ok bool
			if key, ok = k.(*ecdsa.PrivateKey); !ok {
				return nil, nil, errors.New("agent key is not ECDSA")
			}
		case "CERTIFICATE":
			if cert, err = x509.ParseCertificate(blk.Bytes); err != nil {
				return nil, nil, fmt.Errorf("parse agent certificate: %w", err)
			}
		}
	}
	if key == nil || cert == nil {
		return nil, nil, fmt.Errorf("%s: missing key or certificate", path)
	}
	return key, cert, nil
}

func saveEnrollment(dir string, cfg config, key *ecdsa.PrivateKey, certDER, caDER []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, fileCA), pemBytes("CERTIFICATE", caDER), 0o644); err != nil {
		return err
	}
	if err := saveCredentials(dir, key, certDER); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	// Written last: its presence means enrollment completed.
	return writeAtomic(filepath.Join(dir, fileConfig), b, 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func pemBytes(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func readPEM(path, typ string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("%s: expected PEM %s", path, typ)
	}
	return blk.Bytes, nil
}

func readCert(path string) (*x509.Certificate, error) {
	der, err := readPEM(path, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}
