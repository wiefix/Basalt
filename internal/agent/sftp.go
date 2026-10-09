package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/sftpd"
)

const (
	fileSFTPUsers   = "sftp-users.json"
	fileSFTPHostKey = "sftp_host_key"
)

// sftpManager runs the built-in SFTP server and keeps its accounts in sync
// with the panel. Accounts are persisted so SFTP works while the panel is down.
type sftpManager struct {
	log       *slog.Logger
	listen    string
	stateFile string
	srv       *sftpd.Server
	hostKey   ssh.Signer

	mu       sync.Mutex
	revision int64
	port     int
}

func newSFTPManager(cfg Config, sites *sitesManager, log *slog.Logger) (*sftpManager, error) {
	m := &sftpManager{log: log, listen: cfg.SFTPListen, stateFile: filepath.Join(cfg.DataDir, fileSFTPUsers)}
	if m.listen == "" {
		return m, nil
	}
	key, err := sftpd.LoadOrCreateHostKey(filepath.Join(cfg.DataDir, fileSFTPHostKey))
	if err != nil {
		return nil, err
	}
	m.hostKey = key
	hasher := auth.NewHasher(auth.DefaultArgon2Params, min(2, runtime.NumCPU()))
	m.srv = sftpd.New(log, key, hasher, func(id int64) string {
		d, _ := sites.dir(id)
		return d
	})
	return m, nil
}

func (m *sftpManager) enabled() bool { return m.srv != nil }

// Fingerprint is the SHA256 host key fingerprint shown to users.
func (m *sftpManager) Fingerprint() string {
	if m.hostKey == nil {
		return ""
	}
	return ssh.FingerprintSHA256(m.hostKey.PublicKey())
}

func (m *sftpManager) Port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

func (m *sftpManager) run(ctx context.Context) {
	if !m.enabled() {
		return
	}
	if err := m.loadSaved(); err != nil {
		m.log.Error("load saved sftp accounts", "err", err)
	}
	ln, err := net.Listen("tcp", m.listen)
	if err != nil {
		m.log.Error("sftp disabled: cannot listen", "addr", m.listen, "err", err)
		return
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	m.mu.Lock()
	m.port, _ = strconv.Atoi(port)
	m.mu.Unlock()
	if err := m.srv.Serve(ctx, ln); err != nil {
		m.log.Error("sftp server stopped", "err", err)
	}
}

func (m *sftpManager) loadSaved() error {
	b, err := os.ReadFile(m.stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st agentproto.SFTPApply
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse %s: %w", m.stateFile, err)
	}
	m.mu.Lock()
	m.revision = st.Revision
	m.mu.Unlock()
	m.srv.SetUsers(st.Users)
	return nil
}

func (m *sftpManager) Apply(st agentproto.SFTPApply) error {
	if !m.enabled() {
		return errors.New("sftp is disabled on this server")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.Revision < m.revision {
		return nil // outdated delivery
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.stateFile, b, 0o600); err != nil {
		return err
	}
	m.revision = st.Revision
	m.srv.SetUsers(st.Users)
	m.log.Info("sftp accounts applied", "revision", st.Revision, "accounts", len(st.Users))
	return nil
}
