package server

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/store"
)

// TLS on the public ports of container databases: the panel keeps a CA of
// its own for them (not the agents' CA: a client trusting it to check a
// database must not trust agent certificates), in the database with its
// key sealed, so it reaches standby panels and panel backups. Each server
// presents its own certificate for its addresses; users check it against
// the CA (sslmode=verify-full, --ssl-ca).
const (
	settingDBCA = "db_ca"
	// dbTLSValidity is how long a server's certificate is valid; it is
	// renewed when a third is left.
	dbTLSValidity = 365 * 24 * time.Hour
	dbTLSEvery    = 24 * time.Hour
)

var dbCAAD = []byte("db-ca")

func dbTLSAD(serverID int64) []byte { return fmt.Appendf(nil, "db-tls:server:%d", serverID) }

type storedCA struct {
	Cert string `json:"cert"` // base64 DER
	Key  string `json:"key"`  // base64 PKCS#8, sealed
}

// dbCA returns the database CA, creating it on first use.
func (s *Server) dbCA(ctx context.Context) (*pki.CA, error) {
	v, err := s.db.Setting(ctx, settingDBCA)
	if errors.Is(err, store.ErrNotFound) {
		if err := s.newDBCA(ctx); err != nil {
			return nil, err
		}
		v, err = s.db.Setting(ctx, settingDBCA)
	}
	if err != nil {
		return nil, err
	}
	var st storedCA
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return nil, fmt.Errorf("database CA: %w", err)
	}
	cert, err1 := base64.StdEncoding.DecodeString(st.Cert)
	sealed, err2 := base64.StdEncoding.DecodeString(st.Key)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("database CA: %w", err)
	}
	key, err := s.box.Open(sealed, dbCAAD)
	if err != nil {
		return nil, fmt.Errorf("database CA key: %w", err)
	}
	return pki.LoadCA(cert, key)
}

// newDBCA stores a new database CA unless there is one: of two requests
// creating it at once, the first one written wins.
func (s *Server) newDBCA(ctx context.Context) error {
	ca, err := pki.NewCA("Basalt databases", s.now())
	if err != nil {
		return err
	}
	key, err := ca.MarshalKey()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(storedCA{
		Cert: base64.StdEncoding.EncodeToString(ca.Cert.Raw),
		Key:  base64.StdEncoding.EncodeToString(s.box.Seal(key, dbCAAD)),
	})
	return s.db.SetSettingIfAbsent(ctx, settingDBCA, string(b))
}

// dbCACert is the database CA's certificate, PEM.
func (s *Server) dbCACert(ctx context.Context) (string, error) {
	ca, err := s.dbCA(ctx)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})), nil
}

// dbTLSAddrs are the addresses a server's certificate names: its public
// ones and the one its agent connects from.
func dbTLSAddrs(srv *store.Server) []net.IP {
	addrs := srv.DNSAddrs()
	if a, err := netip.ParseAddr(srv.LastIP); err == nil {
		addrs = append(addrs, a.Unmap())
	}
	slices.SortFunc(addrs, netip.Addr.Compare)
	addrs = slices.Compact(addrs)
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.AsSlice()
	}
	return out
}

// serverDBTLS returns the certificate a server presents on its public
// database ports, issuing a new one when there is none, it is a third from
// expiring, the server's addresses changed or the CA did. nil: the server
// has no known address to name.
func (s *Server) serverDBTLS(ctx context.Context, srv *store.Server) (*agentproto.DBExposeTLS, error) {
	ips := dbTLSAddrs(srv)
	if len(ips) == 0 {
		return nil, nil
	}
	ca, err := s.dbCA(ctx)
	if err != nil {
		return nil, err
	}
	certDER, sealed, err := s.db.DBTLS(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	var keyDER []byte
	if cert, err := x509.ParseCertificate(certDER); err == nil && dbTLSCurrent(cert, ca, ips, s.now()) {
		keyDER, err = s.box.Open(sealed, dbTLSAD(srv.ID))
		if err != nil {
			keyDER = nil // issue a new one
		}
	}
	if keyDER == nil {
		der, key, err := ca.IssueServer(fmt.Sprintf("server-%d", srv.ID), ips, nil, s.now(), dbTLSValidity)
		if err != nil {
			return nil, fmt.Errorf("issue the database certificate: %w", err)
		}
		if keyDER, err = x509.MarshalPKCS8PrivateKey(key); err != nil {
			return nil, err
		}
		if err := s.db.SetDBTLS(ctx, srv.ID, der, s.box.Seal(keyDER, dbTLSAD(srv.ID))); err != nil {
			return nil, err
		}
		certDER = der
	}
	return &agentproto.DBExposeTLS{
		Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		Key:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}, nil
}

// dbTLSCurrent reports whether a server's certificate can stay.
func dbTLSCurrent(cert *x509.Certificate, ca *pki.CA, ips []net.IP, now time.Time) bool {
	if cert.CheckSignatureFrom(ca.Cert) != nil || now.After(cert.NotAfter.Add(-dbTLSValidity/3)) {
		return false
	}
	return slices.EqualFunc(cert.IPAddresses, ips, net.IP.Equal) && len(cert.DNSNames) == 0
}

// dbTLSLoop gives agents a new certificate before theirs expires, or after
// a server's addresses change: each push checks it.
func (s *Server) dbTLSLoop(ctx context.Context) {
	t := time.NewTimer(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(dbTLSEvery)
		}
		servers, err := s.db.ListServers(ctx)
		if err != nil {
			s.log.Warn("list servers for database certificates", "err", err)
			continue
		}
		for _, sv := range servers {
			if !s.agents.Connected(sv.ID) {
				continue
			}
			if apps, err := s.db.PublicDatabases(ctx, sv.ID); err != nil || len(apps) == 0 {
				continue
			}
			if _, err := s.PushDBExpose(ctx, sv.ID); err != nil && ctx.Err() == nil {
				s.log.Warn("push public databases", "server_id", sv.ID, "err", err)
			}
		}
	}
}
