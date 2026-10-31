package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
)

// testDBCert issues a certificate for 127.0.0.1 from a new CA.
func testDBCert(t *testing.T) (*agentproto.DBExposeTLS, *x509.CertPool, tls.Certificate) {
	t.Helper()
	ca, err := pki.NewCA("test databases", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	der, key, err := ca.IssueServer("test", []net.IP{net.IPv4(127, 0, 0, 1)}, nil, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &agentproto.DBExposeTLS{Cert: string(certPEM), Key: string(keyPEM)}, ca.Pool(), pair
}

// exposeTLS serves one public database speaking protocol in front of the
// container at target and returns its port and the CA its certificate
// chains to.
func exposeTLS(t *testing.T, protocol string, require bool, target string) (int, *x509.CertPool) {
	t.Helper()
	cert, pool, _ := testDBCert(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resolve := func(context.Context, int64, string, int) (string, error) { return target, nil }
	m := newDBExposeManager(Config{DataDir: t.TempDir()}, resolve, slog.New(slog.DiscardHandler))
	m.start(ctx)
	port := freeTCPPort(t)
	_, err := m.Apply(agentproto.DBExposeApply{TLS: cert, Entries: []agentproto.DBExpose{{
		AppID: 1, Service: "db", TargetPort: 1, Port: port, Allow: []string{"127.0.0.1"}, Protocol: protocol, RequireTLS: require,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return port, pool
}

func dialDB(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

func clientTLS(t *testing.T, c net.Conn, pool *x509.CertPool, alpn ...string) *tls.Conn {
	t.Helper()
	tc := tls.Client(c, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", NextProtos: alpn})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	return tc
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func pgRequest(code uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, 8)
	binary.BigEndian.PutUint32(b[4:], code)
	return b
}

// pgStartup stands for a startup message: anything but the requests.
var pgStartup = []byte{0, 0, 0, 9, 0, 3, 0, 0, 0}

func TestDBTLSPostgres(t *testing.T) {
	port, pool := exposeTLS(t, agentproto.DBProtocolPostgres, false, echoServer(t))

	// SSLRequest, after a GSSENCRequest refused.
	c := dialDB(t, port)
	c.Write(pgRequest(pgGSSENCRequest))
	if b := readN(t, c, 1); string(b) != "N" {
		t.Fatalf("GSSENCRequest answered %q", b)
	}
	c.Write(pgRequest(pgSSLRequest))
	if b := readN(t, c, 1); string(b) != "S" {
		t.Fatalf("SSLRequest answered %q", b)
	}
	if err := echoesOn(clientTLS(t, c, pool, "postgresql")); err != nil {
		t.Fatalf("over TLS: %v", err)
	}

	// Direct TLS (PostgreSQL 17).
	if err := echoesOn(clientTLS(t, dialDB(t, port), pool, "postgresql")); err != nil {
		t.Fatalf("direct TLS: %v", err)
	}

	// Without TLS, while it is not required.
	c = dialDB(t, port)
	c.Write(pgStartup)
	if b := readN(t, c, len(pgStartup)); !bytes.Equal(b, pgStartup) {
		t.Fatalf("plain startup forwarded as %v", b)
	}

	// Bytes after the SSLRequest, before the handshake, end the connection.
	c = dialDB(t, port)
	c.Write(append(pgRequest(pgSSLRequest), pgStartup...))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("data injected before the TLS handshake accepted")
	}

	// Required: a plain startup gets an error; a cancel request passes.
	port, _ = exposeTLS(t, agentproto.DBProtocolPostgres, true, echoServer(t))
	c = dialDB(t, port)
	c.Write(pgStartup)
	if b, _ := io.ReadAll(c); len(b) == 0 || b[0] != 'E' || !bytes.Contains(b, []byte("TLS connections only")) {
		t.Fatalf("plain startup where TLS is required answered %q", b)
	}
	c = dialDB(t, port)
	cancelReq := make([]byte, 16)
	binary.BigEndian.PutUint32(cancelReq, 16)
	binary.BigEndian.PutUint32(cancelReq[4:], pgCancelRequest)
	c.Write(cancelReq)
	if b := readN(t, c, 16); !bytes.Equal(b, cancelReq) {
		t.Fatal("cancel request not forwarded")
	}
}

func TestDBTLSRedis(t *testing.T) {
	port, pool := exposeTLS(t, agentproto.DBProtocolRedis, false, echoServer(t))
	if err := echoesOn(clientTLS(t, dialDB(t, port), pool)); err != nil {
		t.Fatalf("over TLS: %v", err)
	}
	if err := echoes(port); err != nil {
		t.Fatalf("plain: %v", err)
	}
	port, pool = exposeTLS(t, agentproto.DBProtocolRedis, true, echoServer(t))
	if err := echoesOn(clientTLS(t, dialDB(t, port), pool)); err != nil {
		t.Fatalf("over TLS, required: %v", err)
	}
	c := dialDB(t, port)
	c.Write([]byte("PING\r\n"))
	if b, _ := io.ReadAll(c); !bytes.HasPrefix(b, []byte("-ERR")) {
		t.Fatalf("plain where TLS is required answered %q", b)
	}
}

// mysqlPacket frames payload with sequence number seq.
func mysqlPacket(seq byte, payload []byte) []byte {
	return append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}, payload...)
}

func mysqlGreeting(ssl bool) []byte {
	caps := uint16(0x0200) // CLIENT_PROTOCOL_41
	if ssl {
		caps |= mysqlClientSSL
	}
	p := append([]byte{10}, "8.4.0\x00"...)
	p = append(p, 1, 0, 0, 0)                   // connection id
	p = append(p, "12345678"...)                // auth data
	p = append(p, 0, byte(caps), byte(caps>>8)) // filler, capabilities
	p = append(p, make([]byte, 10)...)
	return mysqlPacket(0, p)
}

// mysqlServer stands in for a MySQL container: it greets, switches to TLS
// with its own certificate when the client asks, then echoes.
func mysqlServer(t *testing.T, ssl bool) string {
	t.Helper()
	_, _, own := testDBCert(t) // a CA the agent does not know
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write(mysqlGreeting(ssl))
				p, err := readMySQLPacket(c)
				if err != nil {
					return
				}
				var conn net.Conn = c
				if binary.LittleEndian.Uint32(p[4:])&mysqlClientSSL != 0 {
					tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{own}})
					if tc.Handshake() != nil {
						return
					}
					conn = tc
				} else {
					c.Write(p) // the handshake response, back
				}
				io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestDBTLSMySQL(t *testing.T) {
	sslRequest := mysqlPacket(1, append(binary.LittleEndian.AppendUint32(nil, 0x0200|mysqlClientSSL), make([]byte, 28)...))
	response := mysqlPacket(1, append(binary.LittleEndian.AppendUint32(nil, 0x0200), "user\x00........"...))

	port, pool := exposeTLS(t, agentproto.DBProtocolMySQL, true, mysqlServer(t, true))
	c := dialDB(t, port)
	g, err := readMySQLPacket(c)
	if err != nil {
		t.Fatal(err)
	}
	if ssl, ok := mysqlGreetingSSL(g); !ok || !ssl {
		t.Fatal("the greeting does not offer TLS")
	}
	c.Write(sslRequest)
	tc := clientTLS(t, c, pool) // the agent's certificate, not the container's
	if err := echoesOn(tc); err != nil {
		t.Fatalf("over TLS: %v", err)
	}

	// Required: a client going on without TLS gets an error packet.
	c = dialDB(t, port)
	readMySQLPacket(c)
	c.Write(response)
	p, err := readMySQLPacket(c)
	if err != nil || p[3] != 2 || p[4] != 0xff || binary.LittleEndian.Uint16(p[5:]) != mysqlErrSecureTransport {
		t.Fatalf("plain client where TLS is required: %v %v", p, err)
	}

	// Not required: plain goes through.
	port, _ = exposeTLS(t, agentproto.DBProtocolMySQL, false, mysqlServer(t, true))
	c = dialDB(t, port)
	readMySQLPacket(c)
	c.Write(response)
	if b := readN(t, c, len(response)); !bytes.Equal(b, response) {
		t.Fatal("plain handshake response not forwarded")
	}
	if err := echoesOn(c); err != nil {
		t.Fatal(err)
	}

	// A container without TLS: none offered.
	port, _ = exposeTLS(t, agentproto.DBProtocolMySQL, false, mysqlServer(t, false))
	c = dialDB(t, port)
	g, _ = readMySQLPacket(c)
	if ssl, ok := mysqlGreetingSSL(g); !ok || ssl {
		t.Fatal("TLS offered for a container without it")
	}
}
