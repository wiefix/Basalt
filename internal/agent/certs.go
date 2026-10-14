package agent

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

// certStatus reports the newest certificate the proxy holds for each name,
// from any issuer, read from its storage (certificates only, never keys).
func certStatus(proxyDir string, req agentproto.CertStatus) (agentproto.CertStatusResponse, error) {
	out := agentproto.CertStatusResponse{Certs: []agentproto.CertInfo{}}
	if len(req.Names) > agentproto.MaxCertNames {
		return out, errors.New("too many names")
	}
	for _, name := range req.Names {
		if n, err := proxy.NormalizeRouteDomain(name); err != nil || n != name {
			return out, errors.New("invalid name")
		}
		info := agentproto.CertInfo{Name: name}
		// Caddy keeps *.example.com under wildcard_.example.com.
		dir := strings.Replace(name, "*", "wildcard_", 1)
		files, _ := filepath.Glob(filepath.Join(proxyDir, "storage", "certificates", "*", dir, dir+".crt"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			block, _ := pem.Decode(b)
			if block == nil {
				continue
			}
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !leaf.NotAfter.After(info.NotAfter) {
				continue
			}
			info.NotAfter, info.Issuer = leaf.NotAfter, leaf.Issuer.CommonName
		}
		out.Certs = append(out.Certs, info)
	}
	return out, nil
}
