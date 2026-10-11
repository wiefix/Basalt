package e2e

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// sinkMessage is a message an smtpSink received.
type sinkMessage struct {
	User string // authenticated as
	TLS  bool
	From string
	To   []string
	Data string
}

// smtpSink is an SMTP server that requires STARTTLS and AUTH (as a mail
// relay does) and keeps what it receives.
type smtpSink struct {
	ln       net.Listener
	user     string
	password string
	cfg      *tls.Config

	mu   sync.Mutex
	msgs []sinkMessage
}

func startSMTPSink(t *testing.T, addr, user, password string) *smtpSink {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"relay.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln, user: user, password: password,
		cfg: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpSink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpSink) messages() []sinkMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkMessage(nil), s.msgs...)
}

func (s *smtpSink) serve(conn net.Conn) {
	defer func() { conn.Close() }()
	conn.SetDeadline(time.Now().Add(time.Minute))
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(lines ...string) {
		for _, l := range lines {
			w.WriteString(l + "\r\n")
		}
		w.Flush()
	}
	read := func() (string, bool) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	say("220 relay.test ESMTP sink")
	var m sinkMessage
	for {
		line, ok := read()
		if !ok {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		arg := strings.TrimSpace(line[min(len(verb), len(line)):])
		switch {
		case verb == "EHLO" || verb == "HELO":
			if m.TLS {
				say("250-relay.test", "250-AUTH PLAIN LOGIN", "250 8BITMIME")
			} else {
				say("250-relay.test", "250-STARTTLS", "250 8BITMIME")
			}
		case verb == "STARTTLS" && !m.TLS:
			say("220 go ahead")
			tc := tls.Server(conn, s.cfg)
			if tc.Handshake() != nil {
				return
			}
			conn, m.TLS = tc, true
			r, w = bufio.NewReader(tc), bufio.NewWriter(tc)
		case verb == "AUTH" && m.TLS:
			user, pass, ok := s.auth(arg, say, read)
			if !ok || user != s.user || pass != s.password {
				say("535 authentication failed")
				continue
			}
			m.User = user
			say("235 ok")
		case verb == "MAIL":
			if m.User == "" {
				say("530 authentication required")
				continue
			}
			m.From, m.To = arg, nil
			say("250 ok")
		case verb == "RCPT":
			m.To = append(m.To, arg)
			say("250 ok")
		case verb == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".") + "\n")
			}
			m.Data = b.String()
			s.mu.Lock()
			s.msgs = append(s.msgs, m)
			s.mu.Unlock()
			say("250 queued")
		case verb == "RSET" || verb == "NOOP":
			say("250 ok")
		case verb == "QUIT":
			say("221 bye")
			return
		default:
			say("502 not here")
		}
	}
}

// auth reads AUTH PLAIN or LOGIN credentials.
func (s *smtpSink) auth(arg string, say func(...string), read func() (string, bool)) (string, string, bool) {
	mech, initial, _ := strings.Cut(arg, " ")
	next := func(prompt string) string {
		say("334 " + prompt)
		l, _ := read()
		return l
	}
	dec := func(v string) string {
		b, _ := base64.StdEncoding.DecodeString(v)
		return string(b)
	}
	switch strings.ToUpper(mech) {
	case "PLAIN":
		if initial == "" {
			initial = next("")
		}
		parts := strings.Split(dec(initial), "\x00")
		if len(parts) != 3 {
			return "", "", false
		}
		return parts[1], parts[2], true
	case "LOGIN":
		user := initial
		if user == "" {
			user = next(base64.StdEncoding.EncodeToString([]byte("Username:")))
		}
		pass := next(base64.StdEncoding.EncodeToString([]byte("Password:")))
		return dec(user), dec(pass), true
	}
	return "", "", false
}
