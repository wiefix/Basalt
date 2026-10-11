package mailcheck

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
)

// errNXDomain is a name that does not exist.
var errNXDomain = errors.New("no such name")

// resolver asks recursive DNS servers, the way receiving mail servers see
// the records (not Basalt's own authoritative answers).
type resolver struct {
	servers []string // host:port
	timeout time.Duration
}

// systemResolvers reads the host's recursive DNS servers.
func systemResolvers() ([]string, error) {
	cfg, err := mdns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(cfg.Servers) == 0 {
		return nil, errors.New("no DNS resolver is configured on this server")
	}
	out := make([]string, len(cfg.Servers))
	for i, s := range cfg.Servers {
		out[i] = net.JoinHostPort(s, cfg.Port)
	}
	return out, nil
}

// lookup returns the records of type t at name (the answer section,
// CNAMEs followed by the resolver). A name without such records gives no
// error and no records; a name that does not exist gives errNXDomain.
func (r *resolver) lookup(ctx context.Context, name string, t uint16) ([]mdns.RR, error) {
	m := new(mdns.Msg)
	m.SetQuestion(mdns.Fqdn(name), t)
	m.SetEdns0(1232, false)
	var lastErr error
	for _, srv := range r.servers {
		in, err := r.exchange(ctx, m, srv)
		if err != nil {
			lastErr = err
			continue
		}
		switch in.Rcode {
		case mdns.RcodeSuccess:
		case mdns.RcodeNameError:
			return nil, errNXDomain
		default:
			lastErr = fmt.Errorf("%s: %s", strings.TrimSuffix(name, "."), mdns.RcodeToString[in.Rcode])
			continue
		}
		var out []mdns.RR
		for _, rr := range in.Answer {
			if rr.Header().Rrtype == t {
				out = append(out, rr)
			}
		}
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS resolver")
	}
	return nil, lastErr
}

func (r *resolver) exchange(ctx context.Context, m *mdns.Msg, srv string) (*mdns.Msg, error) {
	c := &mdns.Client{Timeout: r.timeout}
	in, _, err := c.ExchangeContext(ctx, m, srv)
	if err == nil && in.Truncated {
		c.Net = "tcp"
		in, _, err = c.ExchangeContext(ctx, m, srv)
	}
	if err != nil {
		return nil, fmt.Errorf("DNS query for %s failed: %w", strings.TrimSuffix(m.Question[0].Name, "."), err)
	}
	return in, nil
}

// txt returns the TXT strings at name, each record's strings joined.
func (r *resolver) txt(ctx context.Context, name string) ([]string, error) {
	rrs, err := r.lookup(ctx, name, mdns.TypeTXT)
	var out []string
	for _, rr := range rrs {
		out = append(out, strings.Join(rr.(*mdns.TXT).Txt, ""))
	}
	return out, err
}

// addrs returns the IPv4 and IPv6 addresses of name.
func (r *resolver) addrs(ctx context.Context, name string) ([]net.IP, error) {
	var out []net.IP
	var firstErr error
	for _, t := range []uint16{mdns.TypeA, mdns.TypeAAAA} {
		rrs, err := r.lookup(ctx, name, t)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, rr := range rrs {
			switch v := rr.(type) {
			case *mdns.A:
				out = append(out, v.A)
			case *mdns.AAAA:
				out = append(out, v.AAAA)
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return nil, firstErr
}

// mx returns the mail exchangers of name, lowest preference first.
func (r *resolver) mx(ctx context.Context, name string) ([]*mdns.MX, error) {
	rrs, err := r.lookup(ctx, name, mdns.TypeMX)
	out := make([]*mdns.MX, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, rr.(*mdns.MX))
	}
	slices.SortStableFunc(out, func(a, b *mdns.MX) int { return cmp.Compare(a.Preference, b.Preference) })
	return out, err
}

func hostOf(fqdn string) string { return strings.ToLower(strings.TrimSuffix(fqdn, ".")) }
