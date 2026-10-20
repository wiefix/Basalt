package agentproto

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// Agents of different servers reach some of each other's local services
// through a tunnel: TLS with the certificates the panel issued them
// (mutual, pinned to the panel's CA), then one JSON line naming the
// service, then the bytes of the connection. The panel decides who may
// reach what; MariaDB, for one, never listens beyond the loopback.

// TypePeerApply replaces an agent's tunnel setup (PeerApply).
const TypePeerApply = "peer_apply"

// Services an agent offers through its tunnel.
const (
	ServiceMariaDB = "mariadb" // the local MariaDB socket
)

// PeerApply says which servers may reach which local services, and which
// local listeners forward to a service of another server.
type PeerApply struct {
	Revision int64 `json:"revision"`
	// Services lists, per service, the servers allowed to reach it.
	Services map[string][]int64 `json:"services"`
	Forwards []PeerForward      `json:"forwards"`
}

// PeerForward makes Listen (a loopback address) reach Service on the
// server ServerID, whose tunnel listens at Addr.
type PeerForward struct {
	Listen   string `json:"listen"`
	ServerID int64  `json:"server_id"`
	Addr     string `json:"addr"`
	Service  string `json:"service"`
}

// TunnelHello is the first line a tunnel client sends.
type TunnelHello struct {
	Service string `json:"service"`
}

// TunnelReply answers it: OK, or why not.
type TunnelReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// MaxForwards bounds the forwards of one agent.
const MaxForwards = 256

func knownService(s string) bool { return s == ServiceMariaDB }

// Validate checks a tunnel setup: known services, positive server ids,
// forwards listening on the loopback only.
func (p *PeerApply) Validate() error {
	for svc, ids := range p.Services {
		if !knownService(svc) {
			return fmt.Errorf("unknown service %q", svc)
		}
		for _, id := range ids {
			if id <= 0 {
				return errors.New("server ids must be positive")
			}
		}
	}
	if len(p.Forwards) > MaxForwards {
		return errors.New("too many forwards")
	}
	seen := map[string]bool{}
	for _, f := range p.Forwards {
		if !knownService(f.Service) {
			return fmt.Errorf("unknown service %q", f.Service)
		}
		if f.ServerID <= 0 {
			return errors.New("forward: server id required")
		}
		ap, err := netip.ParseAddrPort(f.Listen)
		if err != nil || !ap.Addr().IsLoopback() || ap.Port() < 1024 {
			return fmt.Errorf("forward: %q is not a loopback address with a port from 1024", f.Listen)
		}
		if seen[f.Listen] {
			return fmt.Errorf("forward: %s twice", f.Listen)
		}
		seen[f.Listen] = true
		host, port, err := net.SplitHostPort(f.Addr)
		if err != nil || host == "" {
			return fmt.Errorf("forward: bad address %q", f.Addr)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("forward: bad port in %q", f.Addr)
		}
	}
	return nil
}
