package agent

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// dbNegotiateTimeout bounds the start of a public database connection, up
// to the end of the TLS handshakes: a client that stalls there holds one of
// the limited connection slots.
const dbNegotiateTimeout = 10 * time.Second

// errTLSRequired ends a connection that did not switch to TLS where it is
// required; the client has been told so in its protocol.
var errTLSRequired = errors.New("the client did not use TLS")

// bufConn is a connection whose first bytes were read ahead into r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// dbTLS is how a public database offers TLS to its clients.
type dbTLS struct {
	cert    *tls.Certificate // nil: no TLS of the agent's own
	require bool
}

func (t dbTLS) config(alpn ...string) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{*t.cert}, MinVersion: tls.VersionTLS12, NextProtos: alpn}
}

// negotiate takes a client connection c through the start of the
// database's protocol, switching it to TLS when the client asks, and
// connects to the database's container with dial: after the client's TLS
// handshake (nothing reaches the container before), except for MySQL,
// whose server speaks first. It returns the two connections to forward
// between.
func negotiate(protocol string, t dbTLS, c net.Conn, dial func() (net.Conn, error)) (cc, up net.Conn, err error) {
	c.SetDeadline(time.Now().Add(dbNegotiateTimeout))
	switch protocol {
	case agentproto.DBProtocolMySQL:
		raw, err := dial()
		if err != nil {
			return nil, nil, err
		}
		raw.SetDeadline(time.Now().Add(dbNegotiateTimeout))
		if cc, up, err = negotiateMySQL(t, c, raw); err != nil {
			raw.Close()
			return nil, nil, err
		}
		up.SetDeadline(time.Time{})
	case agentproto.DBProtocolPostgres:
		cc, err = negotiatePostgres(t, c)
	case agentproto.DBProtocolRedis:
		cc, err = negotiateRedis(t, c)
	default:
		cc = c
	}
	if err != nil {
		return nil, nil, err
	}
	if up == nil {
		if up, err = dial(); err != nil {
			return nil, nil, err
		}
	}
	c.SetDeadline(time.Time{})
	return cc, up, nil
}

// handshake runs the server side of TLS on c.
func handshake(c net.Conn, cfg *tls.Config) (net.Conn, error) {
	tc := tls.Server(c, cfg)
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tc, nil
}

// PostgreSQL's requests before the startup message (8 bytes: length, code).
const (
	pgSSLRequest       = 80877103
	pgGSSENCRequest    = 80877104
	pgCancelRequest    = 80877102
	tlsRecordHandshake = 0x16 // first byte of a TLS ClientHello
)

// negotiatePostgres answers SSLRequest (and GSSENCRequest, with no) and
// takes direct TLS (PostgreSQL 17's sslnegotiation=direct, ALPN
// "postgresql"); the container receives the startup message in the clear.
// A request to cancel a query comes without TLS from most clients and
// passes even where TLS is required: it carries no credentials.
func negotiatePostgres(t dbTLS, c net.Conn) (net.Conn, error) {
	br := bufio.NewReader(c)
	for range 3 { // GSSENCRequest, SSLRequest, startup
		first, err := br.Peek(1)
		if err != nil {
			return nil, err
		}
		if first[0] == tlsRecordHandshake && t.cert != nil {
			return handshake(&bufConn{c, br}, t.config("postgresql"))
		}
		hdr, err := br.Peek(8)
		if err != nil {
			return nil, err
		}
		length, code := binary.BigEndian.Uint32(hdr), binary.BigEndian.Uint32(hdr[4:])
		switch {
		case length == 8 && (code == pgSSLRequest || code == pgGSSENCRequest):
			br.Discard(8)
			if code == pgGSSENCRequest || t.cert == nil {
				if _, err := c.Write([]byte("N")); err != nil {
					return nil, err
				}
				continue
			}
			// Bytes sent after the request but before the handshake would
			// reach the server as if they came over TLS (CVE-2021-23214).
			if br.Buffered() > 0 {
				return nil, errors.New("data after SSLRequest")
			}
			if _, err := c.Write([]byte("S")); err != nil {
				return nil, err
			}
			return handshake(c, t.config("postgresql"))
		case length == 16 && code == pgCancelRequest:
			return &bufConn{c, br}, nil
		case t.require:
			c.Write(pgFatal("28000", "this server accepts TLS connections only (sslmode=require or stricter)"))
			return nil, errTLSRequired
		}
		return &bufConn{c, br}, nil
	}
	return nil, errors.New("too many requests before the startup message")
}

// pgFatal is a PostgreSQL ErrorResponse message.
func pgFatal(code, msg string) []byte {
	var body []byte
	for _, f := range [][2]string{{"S", "FATAL"}, {"V", "FATAL"}, {"C", code}, {"M", msg}} {
		body = append(append(append(body, f[0]...), f[1]...), 0)
	}
	body = append(body, 0)
	b := []byte{'E', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(4+len(body)))
	return append(b, body...)
}

// MySQL protocol constants.
const (
	mysqlClientSSL     = 0x0800
	mysqlSSLRequestLen = 32
	mysqlMaxHandshake  = 64 << 10
	// ER_SECURE_TRANSPORT_REQUIRED
	mysqlErrSecureTransport = 3159
)

// readMySQLPacket reads one packet (with its 4-byte header) of the
// connection phase.
func readMySQLPacket(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n > mysqlMaxHandshake {
		return nil, fmt.Errorf("handshake packet of %d bytes", n)
	}
	p := make([]byte, 4+n)
	copy(p, hdr)
	_, err := io.ReadFull(r, p[4:])
	return p, err
}

// mysqlGreetingSSL reports whether a server greeting (protocol 10) offers
// TLS; ok is false for anything else (an error packet, say).
func mysqlGreetingSSL(p []byte) (ssl, ok bool) {
	b := p[4:]
	if len(b) < 1 || b[0] != 10 {
		return false, false
	}
	i := 1
	for i < len(b) && b[i] != 0 { // server version
		i++
	}
	i += 1 + 4 + 8 + 1 // NUL, connection id, auth data, filler
	if i+2 > len(b) {
		return false, false
	}
	return binary.LittleEndian.Uint16(b[i:])&mysqlClientSSL != 0, true
}

// mysqlErr is an error packet with sequence number seq.
func mysqlErr(seq byte, code uint16, msg string) []byte {
	body := []byte{0xff, byte(code), byte(code >> 8), '#'}
	body = append(append(body, "HY000"...), msg...)
	return append([]byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), seq}, body...)
}

// negotiateMySQL passes the container's greeting to the client. A client
// asking for TLS (an SSLRequest packet) is switched to TLS with the agent's
// certificate, and the connection to the container too, with the request
// passed on: the container then sees a secure connection (MySQL's
// caching_sha2_password sends the password in the clear only over one),
// and the packets keep their sequence numbers. The container's own
// certificate (generated by the image) is not checked: it is on the
// host's private network. A container without TLS (MariaDB before 11.4)
// gets no TLS from the agent either: its handshake could not continue.
func negotiateMySQL(t dbTLS, c, up net.Conn) (net.Conn, net.Conn, error) {
	greeting, err := readMySQLPacket(up)
	if err != nil {
		return nil, nil, fmt.Errorf("read the greeting: %w", err)
	}
	ssl, ok := mysqlGreetingSSL(greeting)
	if _, err := c.Write(greeting); err != nil {
		return nil, nil, err
	}
	if !ok {
		return c, up, nil // an error packet: the container closes
	}
	br := bufio.NewReader(c)
	p, err := readMySQLPacket(br)
	if err != nil {
		return nil, nil, err
	}
	asksTLS := len(p) >= 8 && binary.LittleEndian.Uint32(p[4:])&mysqlClientSSL != 0
	switch {
	case asksTLS && ssl && t.cert != nil:
		if len(p) != 4+mysqlSSLRequestLen || br.Buffered() > 0 {
			return nil, nil, errors.New("unexpected data with the TLS request")
		}
		if _, err := up.Write(p); err != nil {
			return nil, nil, err
		}
		tu := tls.Client(up, &tls.Config{InsecureSkipVerify: true}) // the container's own certificate, see above
		if err := tu.Handshake(); err != nil {
			return nil, nil, fmt.Errorf("TLS handshake with the container: %w", err)
		}
		tc, err := handshake(c, t.config())
		if err != nil {
			return nil, nil, err
		}
		return tc, tu, nil
	case !asksTLS && t.require:
		c.Write(mysqlErr(p[3]+1, mysqlErrSecureTransport, "This server accepts TLS connections only (--ssl-mode=REQUIRED or stricter)"))
		return nil, nil, errTLSRequired
	}
	// No TLS of the agent's: what the client sent goes on as it is (with
	// the container's own TLS, end to end, when the client asked for it).
	if _, err := up.Write(p); err != nil {
		return nil, nil, err
	}
	return &bufConn{c, br}, up, nil
}

// negotiateRedis takes a client starting TLS (rediss://) with the
// agent's certificate.
func negotiateRedis(t dbTLS, c net.Conn) (net.Conn, error) {
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return nil, err
	}
	switch {
	case first[0] == tlsRecordHandshake && t.cert != nil:
		return handshake(&bufConn{c, br}, t.config())
	case t.require:
		c.Write([]byte("-ERR this server accepts TLS connections only (rediss://)\r\n"))
		return nil, errTLSRequired
	}
	return &bufConn{c, br}, nil
}
