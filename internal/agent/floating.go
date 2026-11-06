package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

const fileFloating = "floating-ips.json"

// floatingManager keeps the host's floating IPs on its interface: the
// helper puts them there, the set is saved so that they come back when
// the agent starts (they do not survive a reboot) without the panel.
type floatingManager struct {
	log       *slog.Logger
	stateFile string
	priv      interface {
		SetFloatingIPs(ctx context.Context, f agentproto.FloatingIPs) error
	}

	mu    sync.Mutex
	addrs []string
	// applyMu orders the calls to the helper; wake retries a set that
	// could not be applied.
	applyMu sync.Mutex
	wake    chan struct{}
	failed  bool
}

// floatingFunc is a function setting the floating IPs (tests).
type floatingFunc func(ctx context.Context, f agentproto.FloatingIPs) error

func (f floatingFunc) SetFloatingIPs(ctx context.Context, ips agentproto.FloatingIPs) error {
	return f(ctx, ips)
}

func newFloatingManager(cfg Config, priv privileged, log *slog.Logger) *floatingManager {
	m := &floatingManager{log: log, stateFile: filepath.Join(cfg.DataDir, fileFloating), priv: priv,
		wake: make(chan struct{}, 1)}
	if cfg.FloatingIPs != nil {
		m.priv = floatingFunc(cfg.FloatingIPs)
	}
	b, err := os.ReadFile(m.stateFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Error("load floating IPs", "err", err)
	}
	if err == nil {
		var f agentproto.FloatingIPs
		if err := json.Unmarshal(b, &f); err != nil {
			log.Error("load floating IPs", "err", fmt.Errorf("parse %s: %w", m.stateFile, err))
		} else if err := f.Validate(); err != nil {
			log.Error("load floating IPs", "err", err)
		} else {
			m.addrs = f.Addresses
		}
	}
	return m
}

// floatingRetry is how often a set the helper refused is tried again.
const floatingRetry = 30 * time.Second

// run puts the saved set in place, and retries until it is.
func (m *floatingManager) run(ctx context.Context) {
	m.mu.Lock()
	saved := len(m.addrs) > 0
	m.mu.Unlock()
	if saved {
		m.wake <- struct{}{}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-time.After(floatingRetry):
			m.mu.Lock()
			failed := m.failed
			m.mu.Unlock()
			if !failed {
				continue
			}
		}
		if err := m.put(ctx); err != nil {
			m.log.Warn("floating IPs", "err", err)
		}
	}
}

// put gives the helper the current set.
func (m *floatingManager) put(ctx context.Context) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	f := agentproto.FloatingIPs{Addresses: slices.Clone(m.addrs)}
	m.mu.Unlock()
	if f.Addresses == nil {
		f.Addresses = []string{}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	err := m.priv.SetFloatingIPs(ctx, f)
	m.mu.Lock()
	m.failed = err != nil
	m.mu.Unlock()
	return err
}

// Apply replaces the set (from the panel) and puts it in place.
func (m *floatingManager) Apply(ctx context.Context, f agentproto.FloatingIPs) error {
	if err := f.Validate(); err != nil {
		return err
	}
	slices.Sort(f.Addresses)
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.stateFile, b, 0o600); err != nil {
		return publicError(err)
	}
	m.mu.Lock()
	m.addrs = f.Addresses
	m.mu.Unlock()
	return m.put(ctx)
}

// exclude drops the floating IPs from the host's public addresses: they
// are on every server of a group, but automatic DNS records must name
// the server's own.
func (m *floatingManager) exclude(addrs []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.DeleteFunc(addrs, func(a string) bool { return slices.Contains(m.addrs, a) })
}
