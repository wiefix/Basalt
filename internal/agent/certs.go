package agent

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	failures := tlsFailures(proxy.TLSLogPath(proxyDir))
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
		// A failure older than the certificate is history.
		if f, ok := failures[name]; ok && f.at.After(info.NotAfter.AddDate(0, 0, -90)) {
			info.Error, info.ErrorAt = f.msg, f.at
		}
		out.Certs = append(out.Certs, info)
	}
	return out, nil
}

type tlsFailure struct {
	at  time.Time
	msg string
}

// tlsFailures reads the latest certificate error per name from the end of
// the proxy's TLS log.
func tlsFailures(path string) map[string]tlsFailure {
	out := map[string]tlsFailure{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out
	}
	const tail = 1 << 20
	off := max(0, st.Size()-tail)
	buf := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(buf, off)
	for _, line := range bytes.Split(buf[:n], []byte("\n")) {
		var l struct {
			Level       string   `json:"level"`
			TS          float64  `json:"ts"`
			Msg         string   `json:"msg"`
			Identifier  string   `json:"identifier"`
			Identifiers []string `json:"identifiers"`
			Error       string   `json:"error"`
		}
		if json.Unmarshal(line, &l) != nil || l.Level != "error" || l.Error == "" {
			continue
		}
		names := append([]string{l.Identifier}, l.Identifiers...)
		msg := l.Error
		if len(msg) > 500 {
			msg = msg[:500]
		}
		at := time.Unix(int64(l.TS), 0).UTC()
		for _, name := range names {
			if name != "" {
				out[name] = tlsFailure{at: at, msg: strings.ToValidUTF8(msg, "?")}
			}
		}
	}
	return out
}
