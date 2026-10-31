package server

import (
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/store"
)

func TestDBTLSCurrent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ca, _ := pki.NewCA("db", now)
	ips := []net.IP{net.ParseIP("203.0.113.5").To4()}
	der, _, err := ca.IssueServer("server-1", ips, nil, now, dbTLSValidity)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	if !dbTLSCurrent(cert, ca, ips, now) || !dbTLSCurrent(cert, ca, ips, now.Add(dbTLSValidity/2)) {
		t.Fatal("a new certificate is not kept")
	}
	if dbTLSCurrent(cert, ca, ips, now.Add(dbTLSValidity*3/4)) {
		t.Fatal("a certificate a third from expiring is kept")
	}
	if dbTLSCurrent(cert, ca, []net.IP{net.ParseIP("203.0.113.6").To4()}, now) {
		t.Fatal("a certificate for old addresses is kept")
	}
	other, _ := pki.NewCA("db", now)
	if dbTLSCurrent(cert, other, ips, now) {
		t.Fatal("a certificate of another CA is kept")
	}
}

func TestDBTLSAddrs(t *testing.T) {
	srv := &store.Server{LastIP: "::ffff:198.51.100.9"}
	got := dbTLSAddrs(srv)
	if len(got) != 1 || !got[0].Equal(net.ParseIP("198.51.100.9")) {
		t.Fatalf("addresses = %v", got)
	}
}
