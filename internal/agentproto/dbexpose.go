package agentproto

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
)

// TypeDBExpose sets which container databases of the host are reachable
// from outside it (DBExposeApply). The agent itself listens on each port
// and forwards connections from the allowed addresses to the database's
// container: Docker's port publishing would skip the host's firewall and
// cannot restrict who connects. The agent keeps the list on disk, so access
// works while the panel is down. The list is complete: databases missing
// from it stop listening.
//
// With TLS set, the agent also offers TLS on the ports, in each database's
// own protocol (Protocol): PostgreSQL's SSLRequest or direct TLS, MySQL's
// switch to TLS within the handshake, Redis's TLS from the first byte. The
// agent ends the client's TLS itself, so the database containers need no
// certificate; RequireTLS refuses clients that do not switch to TLS.
const TypeDBExpose = "db_expose"

// Limits on public database access.
const (
	MaxDBExpose      = 200
	MaxDBExposeAllow = 50
	// MinDBExposePort keeps public database ports out of the privileged
	// range, where the host's own services listen.
	MinDBExposePort = 1024
)

type DBExposeApply struct {
	Entries []DBExpose `json:"entries"`
	// TLS is the certificate the agent presents to clients; without it
	// connections are forwarded as they come.
	TLS *DBExposeTLS `json:"tls,omitempty"`
}

// DBExposeTLS is a server certificate (chain first) and its key, PEM.
type DBExposeTLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// The protocols a public database speaks (DBExpose.Protocol): how the agent
// offers TLS. Empty forwards connections as they come.
const (
	DBProtocolPostgres = "postgres"
	DBProtocolMySQL    = "mysql" // MySQL and MariaDB
	DBProtocolRedis    = "redis"
)

// DBExpose makes container port TargetPort of service Service of database
// app AppID reachable on host port Port, from the addresses in Allow only.
type DBExpose struct {
	AppID      int64    `json:"app_id"`
	Service    string   `json:"service"`
	TargetPort int      `json:"target_port"`
	Port       int      `json:"port"`
	Allow      []string `json:"allow"` // CIDR prefixes
	Protocol   string   `json:"protocol,omitempty"`
	RequireTLS bool     `json:"require_tls,omitempty"`
}

// DBExposeStatus is the outcome of one entry: Error is why the agent
// cannot listen on its port.
type DBExposeStatus struct {
	AppID int64  `json:"app_id"`
	Error string `json:"error,omitempty"`
}

type DBExposeResponse struct {
	Entries []DBExposeStatus `json:"entries"`
}

// ParseAllow normalises an allow list: addresses become single-address
// prefixes, prefixes lose host bits. It is never empty: a database open to
// any address is asked for by 0.0.0.0/0 and ::/0.
func ParseAllow(list []string) ([]netip.Prefix, error) {
	if len(list) == 0 {
		return nil, errors.New("list the addresses allowed to connect (0.0.0.0/0 and ::/0 for any)")
	}
	if len(list) > MaxDBExposeAllow {
		return nil, fmt.Errorf("at most %d allowed addresses", MaxDBExposeAllow)
	}
	out := make([]netip.Prefix, 0, len(list))
	for _, v := range list {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			a, aerr := netip.ParseAddr(v)
			if aerr != nil || a.Zone() != "" {
				return nil, fmt.Errorf("%q is not an IP address or CIDR prefix", v)
			}
			p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-unmapBits(p)).Masked()
		if p.Bits() < 0 {
			return nil, fmt.Errorf("%q is not an IP address or CIDR prefix", v)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// unmapBits is what an IPv4-mapped prefix loses becoming an IPv4 one.
func unmapBits(p netip.Prefix) int {
	if p.Addr().Is4In6() {
		return 96
	}
	return 0
}

// Validate checks one entry.
func (e *DBExpose) Validate() error {
	switch {
	case e.AppID <= 0 || !ServiceNameRE.MatchString(e.Service):
		return errors.New("invalid database app")
	case e.TargetPort < 1 || e.TargetPort > 65535:
		return errors.New("invalid database port")
	case e.Port < MinDBExposePort || e.Port > 65535:
		return fmt.Errorf("the public port must be between %d and 65535", MinDBExposePort)
	case !slices.Contains([]string{"", DBProtocolPostgres, DBProtocolMySQL, DBProtocolRedis}, e.Protocol):
		return fmt.Errorf("unknown database protocol %q", e.Protocol)
	case e.RequireTLS && e.Protocol == "":
		return errors.New("TLS needs the database's protocol")
	}
	_, err := ParseAllow(e.Allow)
	return err
}

// Validate checks the whole list: one entry per app and per port.
func (r *DBExposeApply) Validate() error {
	if len(r.Entries) > MaxDBExpose {
		return fmt.Errorf("at most %d public databases", MaxDBExpose)
	}
	apps, ports := map[int64]bool{}, map[int]bool{}
	for i := range r.Entries {
		e := &r.Entries[i]
		if err := e.Validate(); err != nil {
			return fmt.Errorf("database %d: %w", e.AppID, err)
		}
		if e.RequireTLS && r.TLS == nil {
			return fmt.Errorf("database %d: TLS is required but there is no certificate", e.AppID)
		}
		if apps[e.AppID] || ports[e.Port] {
			return fmt.Errorf("database %d or port %d is listed twice", e.AppID, e.Port)
		}
		apps[e.AppID], ports[e.Port] = true, true
	}
	if r.TLS != nil && (r.TLS.Cert == "" || r.TLS.Key == "") {
		return errors.New("the TLS certificate or key is missing")
	}
	return nil
}
