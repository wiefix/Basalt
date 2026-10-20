package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
)

// agentCert issues a certificate for a server from ca.
func agentCert(t *testing.T, ca *pki.CA, id int64) tls.Certificate {
	t.Helper()
	key, _ := pki.NewKey()
	csr, _ := pki.NewCSR(key)
	der, err := ca.SignAgent(csr, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// TestTunnel: a server's local listener reaches another server's MariaDB
// socket through the tunnel when the panel allows it, and nothing else.
func TestTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ca, _ := pki.NewCA("test", time.Now())
	other, _ := pki.NewCA("other", time.Now())

	// The "MariaDB" of server 1 echoes lines.
	sock := filepath.Join(os.TempDir(), "bt-"+time.Now().Format("150405.000000")+".sock")
	t.Cleanup(func() { os.Remove(sock) })
	dbl, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer dbl.Close()
	go func() {
		for {
			c, err := dbl.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()

	newManager := func(id int64, cert tls.Certificate, root *pki.CA, listen string) *tunnelManager {
		return newTunnelManager(Config{PeerListen: listen, MySQLSocket: sock}, func() tls.Certificate { return cert },
			func() *x509.Certificate { return root.Cert }, log.With("server", id))
	}
	s1 := newManager(1, agentCert(t, ca, 1), ca, "127.0.0.1:0")
	go s1.run(ctx, s1.open())
	port, errMsg := s1.Status()
	if port == 0 || errMsg != "" {
		t.Fatalf("status: %d %q", port, errMsg)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	s2 := newManager(2, agentCert(t, ca, 2), ca, "")
	go s2.run(ctx, s2.open())
	stranger := newManager(2, agentCert(t, other, 2), ca, "") // trusts server 1, presents a foreign certificate

	free := func() string {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		return l.Addr().String()
	}
	// talk sends a line through a local forward and returns the answer
	// ("" when the tunnel closes the connection).
	talk := func(listen string) string {
		t.Helper()
		c, err := net.DialTimeout("tcp", listen, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("hello\n"))
		line, _ := bufio.NewReader(c).ReadString('\n')
		return line
	}

	fwd := free()
	if err := s2.apply(ctx, agentproto.PeerApply{Revision: 1, Forwards: []agentproto.PeerForward{
		{Listen: fwd, ServerID: 1, Addr: addr, Service: agentproto.ServiceMariaDB}}}); err != nil {
		t.Fatal(err)
	}
	if got := talk(fwd); got != "" {
		t.Fatalf("reached before the panel allowed it: %q", got)
	}
	s1.apply(ctx, agentproto.PeerApply{Revision: 1, Services: map[string][]int64{agentproto.ServiceMariaDB: {2}}})
	if got := talk(fwd); got != "hello\n" {
		t.Fatalf("through the tunnel: %q", got)
	}
	// An older revision changes nothing.
	s1.apply(ctx, agentproto.PeerApply{Revision: 0})
	if got := talk(fwd); got != "hello\n" {
		t.Fatalf("after an old revision: %q", got)
	}

	// The client checks it reached the server it meant: server 1 is not 3.
	wrong := free()
	s2.apply(ctx, agentproto.PeerApply{Revision: 2, Forwards: []agentproto.PeerForward{
		{Listen: fwd, ServerID: 1, Addr: addr, Service: agentproto.ServiceMariaDB},
		{Listen: wrong, ServerID: 3, Addr: addr, Service: agentproto.ServiceMariaDB}}})
	if got := talk(wrong); got != "" {
		t.Fatalf("reached a server other than the one meant: %q", got)
	}
	// A certificate of another CA is refused.
	if _, _, err := stranger.dial(ctx, agentproto.PeerForward{ServerID: 1, Addr: addr, Service: agentproto.ServiceMariaDB}); err == nil {
		t.Fatal("a stranger's certificate got through")
	}
	// Taken back: refused again; a removed forward stops listening.
	s1.apply(ctx, agentproto.PeerApply{Revision: 3, Services: map[string][]int64{agentproto.ServiceMariaDB: {7}}})
	if got := talk(fwd); got != "" {
		t.Fatalf("reached after the panel took it back: %q", got)
	}
	s2.apply(ctx, agentproto.PeerApply{Revision: 3})
	if _, err := net.DialTimeout("tcp", fwd, time.Second); err == nil {
		t.Fatal("a removed forward still listens")
	}

	for _, bad := range []agentproto.PeerApply{
		{Services: map[string][]int64{"ssh": {1}}},
		{Services: map[string][]int64{agentproto.ServiceMariaDB: {0}}},
		{Forwards: []agentproto.PeerForward{{Listen: "0.0.0.0:3307", ServerID: 1, Addr: addr, Service: agentproto.ServiceMariaDB}}},
		{Forwards: []agentproto.PeerForward{{Listen: "127.0.0.1:80", ServerID: 1, Addr: addr, Service: agentproto.ServiceMariaDB}}},
		{Forwards: []agentproto.PeerForward{{Listen: "127.0.0.1:3307", ServerID: 1, Addr: "nohost", Service: agentproto.ServiceMariaDB}}},
		{Forwards: []agentproto.PeerForward{{Listen: "127.0.0.1:3307", Addr: addr, Service: agentproto.ServiceMariaDB}}},
		{Forwards: []agentproto.PeerForward{{Listen: "127.0.0.1:3307", ServerID: 1, Addr: addr, Service: "x"}}},
	} {
		bad.Revision = 9
		if err := s2.apply(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
