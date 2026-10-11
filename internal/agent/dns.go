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
	"sync"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
)

const fileDNSZones = "dns-zones.json"

// dnsManager keeps the DNS zones pushed by the panel and, when a listen
// address is configured, serves them. Zones are persisted so DNS keeps
// answering while the panel is down.
type dnsManager struct {
	log       *slog.Logger
	stateFile string
	srv       *dns.Server

	// The sockets are opened in newDNSManager, so the hello already
	// reports whether DNS is being served.
	sockets   *dns.Sockets
	port      int
	listenErr string

	mu       sync.Mutex
	revision int64
}

func newDNSManager(cfg Config, log *slog.Logger) *dnsManager {
	m := &dnsManager{log: log, stateFile: filepath.Join(cfg.DataDir, fileDNSZones)}
	if cfg.DNSListen == "" {
		return m
	}
	m.srv = dns.NewServer(log, net.DefaultResolver)
	if err := m.loadSaved(); err != nil {
		log.Error("load saved dns zones", "err", err)
	}
	k, err := dns.Listen(context.Background(), cfg.DNSListen)
	if err != nil {
		log.Error("dns disabled: cannot listen", "addr", cfg.DNSListen, "err", err)
		m.listenErr = publicError(err).Error()
		return m
	}
	m.sockets, m.port = k, k.Port()
	return m
}

// Status is the DNS port (0 when not serving) and the listen error.
func (m *dnsManager) Status() (int, string) { return m.port, m.listenErr }

func (m *dnsManager) run(ctx context.Context) {
	if m.sockets == nil {
		return
	}
	m.log.Info("dns server listening", "addrs", m.sockets.Addrs())
	if err := m.srv.Serve(ctx, m.sockets); err != nil {
		m.log.Error("dns server stopped", "err", err)
	}
}

func (m *dnsManager) loadSaved() error {
	b, err := os.ReadFile(m.stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st agentproto.DNSApply
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse %s: %w", m.stateFile, err)
	}
	if err := m.srv.SetZones(st.Zones); err != nil {
		return err
	}
	m.revision = st.Revision
	return nil
}

// Apply stores the zones and serves them. Agents without a DNS listener
// keep them too, so enabling DNS later serves the current zones at once.
func (m *dnsManager) Apply(st agentproto.DNSApply) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.Revision < m.revision {
		return nil // outdated delivery
	}
	if err := dns.Check(st.Zones); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.stateFile, b, 0o600); err != nil {
		return publicError(err)
	}
	m.revision = st.Revision
	if m.srv != nil {
		if err := m.srv.SetZones(st.Zones); err != nil {
			return err
		}
	}
	m.log.Info("dns zones applied", "revision", st.Revision, "zones", len(st.Zones))
	return nil
}
