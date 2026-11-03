package agent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

// certSource gives the newest certificate of each name the proxy holds
// (proxy.Admin.Certificates: the proxy reads its own storage).
type certSource func(ctx context.Context, names []string, keys bool) ([]proxy.Certificate, error)

// certStatus reports the newest certificate the proxy holds for each name,
// from any issuer (certificates only, never keys), and the last failure
// to get one from the proxy's log of certificate events.
func certStatus(ctx context.Context, tlsLog string, certs certSource, req agentproto.CertStatus) (agentproto.CertStatusResponse, error) {
	out := agentproto.CertStatusResponse{Certs: []agentproto.CertInfo{}}
	if len(req.Names) > agentproto.MaxCertNames {
		return out, errors.New("too many names")
	}
	for _, name := range req.Names {
		if n, err := proxy.NormalizeRouteDomain(name); err != nil || n != name {
			return out, errors.New("invalid name")
		}
	}
	held := map[string]proxy.Certificate{}
	if len(req.Names) > 0 {
		list, err := certs(ctx, req.Names, false)
		if err != nil {
			return out, err
		}
		for _, c := range list {
			held[c.Name] = c
		}
	}
	failures := tlsFailures(tlsLog)
	for _, name := range req.Names {
		info := agentproto.CertInfo{Name: name}
		if block, _ := pem.Decode(held[name].Chain); block != nil {
			if leaf, err := x509.ParseCertificate(block.Bytes); err == nil {
				info.NotAfter, info.Issuer = leaf.NotAfter, leaf.Issuer.CommonName
			}
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
