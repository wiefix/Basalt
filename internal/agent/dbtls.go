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
	case agentproto.DBProtocolMongo:
		cc, err = negotiateMongo(t, c)
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

// mysqlGreetingCaps finds the (lower) capability flags of a server
// greeting (protocol 10): their offset in the packet; ok is false for
// anything else (an error packet, say).
func mysqlGreetingCaps(p []byte) (off int, ok bool) {
	b := p[4:]
	if len(b) < 1 || b[0] != 10 {
		return 0, false
	}
	i := 1
	for i < len(b) && b[i] != 0 { // server version
		i++
	}
	i += 1 + 4 + 8 + 1 // NUL, connection id, auth data, filler
	if i+2 > len(b) {
		return 0, false
	}
	return 4 + i, true
}

// mysqlGreetingSSL reports whether a server greeting offers TLS; ok is
// false for anything but a greeting.
func mysqlGreetingSSL(p []byte) (ssl, ok bool) {
	off, ok := mysqlGreetingCaps(p)
	return ok && binary.LittleEndian.Uint16(p[off:])&mysqlClientSSL != 0, ok
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
// host's private network. For a container without TLS (MariaDB 10.11) the
// agent offers TLS in its greeting and ends it alone (mysqlTLSAlone).
func negotiateMySQL(t dbTLS, c, up net.Conn) (net.Conn, net.Conn, error) {
	greeting, err := readMySQLPacket(up)
	if err != nil {
		return nil, nil, fmt.Errorf("read the greeting: %w", err)
	}
	ssl, ok := mysqlGreetingSSL(greeting)
	alone := ok && !ssl && t.cert != nil
	if alone {
		off, _ := mysqlGreetingCaps(greeting)
		binary.LittleEndian.PutUint16(greeting[off:], binary.LittleEndian.Uint16(greeting[off:])|mysqlClientSSL)
	}
	if _, err := c.Write(greeting); err != nil {
		return nil, nil, err
	}
	if !ok {
		return c, up, nil // an error packet: the container closes
	}
	// Read without reading ahead: a client asking for TLS sends its
	// ClientHello right after the request, without waiting for an answer.
	p, err := readMySQLPacket(c)
	if err != nil {
		return nil, nil, err
	}
	asksTLS := len(p) >= 8 && binary.LittleEndian.Uint32(p[4:])&mysqlClientSSL != 0
	if asksTLS && (alone || ssl && t.cert != nil) && len(p) != 4+mysqlSSLRequestLen {
		return nil, nil, errors.New("unexpected data with the TLS request")
	}
	switch {
	case asksTLS && alone:
		tc, err := handshake(c, t.config())
		if err != nil {
			return nil, nil, err
		}
		if err := mysqlTLSAlone(tc, up); err != nil {
			return nil, nil, err
		}
		return tc, up, nil
	case asksTLS && ssl && t.cert != nil:
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
	return c, up, nil
}

// mysqlTLSAlone takes a client that switched to TLS with the agent (tc)
// through the rest of the connection phase with a container that knows
// nothing of it (up): the client numbers its packets one higher (its
// SSLRequest was one) and its handshake response asks for TLS, which the
// container does not have. So the response loses CLIENT_SSL, and packets
// are renumbered both ways until the container's OK or error; the command
// phase numbers each command from 0 again. The container sees a connection
// without TLS: MariaDB 10.11's password methods (mysql_native_password,
// ed25519) never send the password itself.
func mysqlTLSAlone(tc, up net.Conn) error {
	p, err := readMySQLPacket(tc) // the handshake response
	if err != nil {
		return err
	}
	if len(p) < 8 || p[3] != 2 {
		return errors.New("unexpected handshake response")
	}
	binary.LittleEndian.PutUint32(p[4:], binary.LittleEndian.Uint32(p[4:])&^mysqlClientSSL)
	for range 8 { // authentication exchanges
		p[3]--
		if _, err := up.Write(p); err != nil {
			return err
		}
	container:
		for {
			if p, err = readMySQLPacket(up); err != nil {
				return err
			}
			p[3]++
			if _, err := tc.Write(p); err != nil {
				return err
			}
			switch {
			case len(p) > 4 && (p[4] == 0x00 || p[4] == 0xff): // OK, error
				return nil
			case len(p) != 6 || p[4] != 0x01 || p[5] != 0x03:
				break container // the client answers
			}
			// Fast authentication succeeded: an OK follows.
		}
		if p, err = readMySQLPacket(tc); err != nil {
			return err
		}
	}
	return errors.New("too many authentication exchanges")
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

// MongoDB's wire protocol: a message header (little-endian int32 length,
// request ID, ID responded to, opcode); clients start with a hello, as
// OP_QUERY (most drivers) or OP_MSG.
const (
	mongoOpReply  = 1
	mongoOpQuery  = 2004
	mongoOpMsg    = 2013
	mongoMaxHello = 64 << 10
)

// negotiateMongo takes a client starting TLS (tls=true) with the agent's
// certificate. Where TLS is required, a client without it gets an error
// in reply to its hello.
func negotiateMongo(t dbTLS, c net.Conn) (net.Conn, error) {
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return nil, err
	}
	switch {
	case first[0] == tlsRecordHandshake && t.cert != nil:
		return handshake(&bufConn{c, br}, t.config())
	case t.require:
		if reply := mongoRefusal(br, "this server accepts TLS connections only (tls=true)"); reply != nil {
			c.Write(reply)
		}
		return nil, errTLSRequired
	}
	return &bufConn{c, br}, nil
}

// mongoRefusal reads a client's first message and returns an error reply
// to it ({ok: 0, errmsg: msg}), or nil when it is no hello.
func mongoRefusal(r io.Reader, msg string) []byte {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(hdr))
	op := binary.LittleEndian.Uint32(hdr[12:])
	if n < 16 || n > mongoMaxHello || op != mongoOpQuery && op != mongoOpMsg {
		return nil
	}
	// All of it: closing with data unread would reset the connection and
	// lose the reply.
	if _, err := io.CopyN(io.Discard, r, int64(n-16)); err != nil {
		return nil
	}
	body, replyOp := make([]byte, 20), uint32(mongoOpReply) // flags, cursor ID, starting from, count
	binary.LittleEndian.PutUint32(body[16:], 1)
	if op == mongoOpMsg {
		body, replyOp = make([]byte, 5), mongoOpMsg // flags, a body section
	}
	body = append(body, bsonError(msg)...)
	reply := make([]byte, 16, 16+len(body))
	binary.LittleEndian.PutUint32(reply, uint32(16+len(body)))
	binary.LittleEndian.PutUint32(reply[4:], 1) // request ID
	copy(reply[8:12], hdr[4:8])                 // responding to the hello
	binary.LittleEndian.PutUint32(reply[12:], replyOp)
	return append(reply, body...)
}

// bsonError is the BSON document {ok: 0.0, errmsg: msg}.
func bsonError(msg string) []byte {
	b := append([]byte{0, 0, 0, 0, 0x01}, "ok\x00"...) // length, a double
	b = append(b, make([]byte, 8)...)
	b = append(append(b, 0x02), "errmsg\x00"...) // a string
	b = binary.LittleEndian.AppendUint32(b, uint32(len(msg)+1))
	b = append(append(b, msg...), 0, 0)
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	return b
}
