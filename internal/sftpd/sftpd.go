// Package sftpd is the SFTP server built into the agent. Each account is
// jailed to one site directory (seen as "/"); file access goes through the
// files package, so symlinks and ".." cannot leave the site. Only the sftp
// subsystem is offered: no shell, exec or port forwarding.
package sftpd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/files"
)

const (
	maxConnsPerIP = 10
	maxConns      = 500
	idleTimeout   = 15 * time.Minute
	handshakeTime = 30 * time.Second
)

// User is an SFTP account as delivered by the panel.
type User struct {
	Name         string   `json:"name"`
	SiteID       int64    `json:"site_id"`
	PasswordHash string   `json:"password_hash,omitempty"` // argon2id; empty disables password login
	PublicKeys   []string `json:"public_keys,omitempty"`   // authorized_keys format
}

type account struct {
	User
	keys [][]byte // wire-format public keys
}

type Server struct {
	log     *slog.Logger
	hostKey ssh.Signer
	hasher  *auth.Hasher
	siteDir func(siteID int64) string

	ipLimiter   *auth.Limiter
	userLimiter *auth.Limiter

	mu       sync.RWMutex
	accounts map[string]*account

	connMu sync.Mutex
	perIP  map[string]int
	total  int
}

func New(log *slog.Logger, hostKey ssh.Signer, hasher *auth.Hasher, siteDir func(int64) string) *Server {
	return &Server{
		log: log, hostKey: hostKey, hasher: hasher, siteDir: siteDir,
		ipLimiter:   auth.NewLimiter(20, 3*time.Second),
		userLimiter: auth.NewLimiter(10, 6*time.Second),
		accounts:    map[string]*account{},
		perIP:       map[string]int{},
	}
}

// SetUsers replaces the account list. Invalid public keys are skipped.
func (s *Server) SetUsers(users []User) {
	m := make(map[string]*account, len(users))
	for _, u := range users {
		a := &account{User: u}
		for _, k := range u.PublicKeys {
			pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
			if err != nil {
				s.log.Warn("skipping invalid SFTP public key", "user", u.Name)
				continue
			}
			a.keys = append(a.keys, pk.Marshal())
		}
		m[u.Name] = a
	}
	s.mu.Lock()
	s.accounts = m
	s.mu.Unlock()
}

func (s *Server) lookup(name string) *account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accounts[name]
}

func (s *Server) config(ip string) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if !s.userLimiter.Allow(c.User()) {
				return nil, errors.New("rate limited")
			}
			a := s.lookup(c.User())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if a == nil || a.PasswordHash == "" {
				s.hasher.VerifyDummy(ctx, string(pw))
				s.log.Info("sftp login failed", "user", c.User(), "ip", ip)
				return nil, errors.New("denied")
			}
			ok, _, err := s.hasher.Verify(ctx, a.PasswordHash, string(pw))
			if err != nil || !ok {
				s.log.Info("sftp login failed", "user", c.User(), "ip", ip)
				return nil, errors.New("denied")
			}
			return permissions(a), nil
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			a := s.lookup(c.User())
			if a != nil {
				wire := key.Marshal()
				for _, k := range a.keys {
					if subtle.ConstantTimeCompare(k, wire) == 1 {
						return permissions(a), nil
					}
				}
			}
			return nil, errors.New("denied")
		},
		ServerVersion: "SSH-2.0-Basalt",
	}
	cfg.AddHostKey(s.hostKey)
	return cfg
}

func permissions(a *account) *ssh.Permissions {
	return &ssh.Permissions{Extensions: map[string]string{"site_id": strconv.FormatInt(a.SiteID, 10)}}
}

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	s.log.Info("sftp listening", "addr", ln.Addr().String())
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		if !s.ipLimiter.Allow(ip) || !s.acquire(ip) {
			c.Close()
			continue
		}
		go func() {
			defer s.release(ip)
			s.handle(ctx, c, ip)
		}()
	}
}

func (s *Server) acquire(ip string) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.total >= maxConns || s.perIP[ip] >= maxConnsPerIP {
		return false
	}
	s.total++
	s.perIP[ip]++
	return true
}

func (s *Server) release(ip string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.total--
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

// idleConn closes connections that see no traffic for idleTimeout.
type idleConn struct{ net.Conn }

func (c idleConn) Read(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Read(b)
}

func (s *Server) handle(ctx context.Context, raw net.Conn, ip string) {
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(handshakeTime))
	conn, chans, reqs, err := ssh.NewServerConn(raw, s.config(ip))
	if err != nil {
		return
	}
	defer conn.Close()
	raw.SetDeadline(time.Time{})
	go ssh.DiscardRequests(reqs) // no global requests (tcpip-forward etc.)

	siteID, _ := strconv.ParseInt(conn.Permissions.Extensions["site_id"], 10, 64)
	user := conn.User()
	s.log.Info("sftp login", "user", user, "site_id", siteID, "ip", ip)
	go func() { <-ctx.Done(); conn.Close() }()

	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.Prohibited, "only sftp is available")
			continue
		}
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.session(ch, requests, siteID, user)
	}
}

func (s *Server) session(ch ssh.Channel, requests <-chan *ssh.Request, siteID int64, user string) {
	defer ch.Close()
	for req := range requests {
		ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
		req.Reply(ok, nil)
		if !ok {
			continue // shell, exec, pty, env: refused
		}
		site, err := files.Open(s.siteDir(siteID))
		if err != nil {
			s.log.Warn("sftp: site unavailable", "site_id", siteID, "err", err)
			return
		}
		h := &handlers{site: site}
		srv := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
		err = srv.Serve()
		srv.Close()
		site.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) && err.Error() != "EOF" {
			s.log.Debug("sftp session ended", "user", user, "err", err)
		}
		return
	}
}

// LoadOrCreateHostKey reads an ed25519 host key, generating it on first use.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		blk, err := ssh.MarshalPrivateKey(priv, "basalt sftp host key")
		if err != nil {
			return nil, err
		}
		b = pem.EncodeToMemory(blk)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse sftp host key: %w", err)
	}
	return signer, nil
}
