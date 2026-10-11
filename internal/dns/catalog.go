package dns

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	mdns "github.com/miekg/dns"
)

// catalog is the compiled, read-only form of all zones a server answers for.
type catalog struct {
	zones map[string]*zoneData // by name, lowercase without trailing dot
}

type zoneData struct {
	name string
	soa  *mdns.SOA
	// nodes holds every owner name with records; names that exist only
	// because something below them does (empty non-terminals) are in
	// nodes too, with no records.
	nodes map[string]*node
}

type node struct {
	rrsets map[uint16][]mdns.RR
	// alias is the ALIAS target ("" if none) and its TTL.
	alias    string
	aliasTTL uint32
	// cut marks a delegation (NS below the apex).
	cut bool
}

func fqdn(name string) string { return name + "." }

// compile validates zones and builds the lookup structure.
func compile(zones []Zone) (*catalog, error) {
	c := &catalog{zones: make(map[string]*zoneData, len(zones))}
	for i := range zones {
		z := &zones[i]
		if err := z.Validate(); err != nil {
			return nil, fmt.Errorf("zone %s: %w", z.Name, err)
		}
		if c.zones[z.Name] != nil {
			return nil, fmt.Errorf("zone %s listed twice", z.Name)
		}
		zd, err := compileZone(z)
		if err != nil {
			return nil, fmt.Errorf("zone %s: %w", z.Name, err)
		}
		c.zones[z.Name] = zd
	}
	return c, nil
}

func compileZone(z *Zone) (*zoneData, error) {
	zd := &zoneData{name: z.Name, nodes: map[string]*node{}}
	mname := z.Name
	if ns := z.Nameservers(); len(ns) > 0 {
		mname = ns[0]
	}
	zd.soa = &mdns.SOA{
		Hdr:     mdns.RR_Header{Name: fqdn(z.Name), Rrtype: mdns.TypeSOA, Class: mdns.ClassINET, Ttl: soaTTL},
		Ns:      fqdn(mname),
		Mbox:    fqdn("hostmaster." + z.Name),
		Serial:  z.Serial,
		Refresh: soaRefresh, Retry: soaRetry, Expire: soaExpire, Minttl: NegativeTTL,
	}
	apex := zd.ensure(z.Name)
	apex.rrsets[mdns.TypeSOA] = []mdns.RR{zd.soa}
	for i := range z.Records {
		r := &z.Records[i]
		owner := r.FQDN(z.Name)
		n := zd.ensure(owner)
		// Empty non-terminals: every name between the owner and the apex exists.
		for p := owner; p != z.Name; {
			_, p, _ = strings.Cut(p, ".")
			zd.ensure(p)
		}
		if r.Type == TypeALIAS {
			n.alias, n.aliasTTL = r.Value, r.TTL
			continue
		}
		rr, err := r.rr(owner)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", r.Name, r.Type, err)
		}
		t := rr.Header().Rrtype
		n.rrsets[t] = append(n.rrsets[t], rr)
		if t == mdns.TypeNS && owner != z.Name {
			n.cut = true
		}
	}
	return zd, nil
}

func (zd *zoneData) ensure(name string) *node {
	n := zd.nodes[name]
	if n == nil {
		n = &node{rrsets: map[uint16][]mdns.RR{}}
		zd.nodes[name] = n
	}
	return n
}

// rr builds the wire record from validated fields.
func (r *Record) rr(owner string) (mdns.RR, error) {
	hdr := func(t uint16) mdns.RR_Header {
		return mdns.RR_Header{Name: fqdn(owner), Rrtype: t, Class: mdns.ClassINET, Ttl: r.TTL}
	}
	target := func() string {
		if r.Value == "." {
			return "."
		}
		return fqdn(r.Value)
	}
	switch r.Type {
	case TypeA:
		a := netip.MustParseAddr(r.Value).As4()
		return &mdns.A{Hdr: hdr(mdns.TypeA), A: net.IP(a[:])}, nil
	case TypeAAAA:
		a := netip.MustParseAddr(r.Value).As16()
		return &mdns.AAAA{Hdr: hdr(mdns.TypeAAAA), AAAA: net.IP(a[:])}, nil
	case TypeCNAME:
		return &mdns.CNAME{Hdr: hdr(mdns.TypeCNAME), Target: target()}, nil
	case TypeNS:
		return &mdns.NS{Hdr: hdr(mdns.TypeNS), Ns: target()}, nil
	case TypePTR:
		return &mdns.PTR{Hdr: hdr(mdns.TypePTR), Ptr: target()}, nil
	case TypeMX:
		return &mdns.MX{Hdr: hdr(mdns.TypeMX), Preference: r.Priority, Mx: target()}, nil
	case TypeSRV:
		return &mdns.SRV{Hdr: hdr(mdns.TypeSRV), Priority: r.Priority, Weight: r.Weight, Port: r.Port, Target: target()}, nil
	case TypeTXT:
		return &mdns.TXT{Hdr: hdr(mdns.TypeTXT), Txt: splitTXT(r.Value)}, nil
	case TypeCAA:
		return &mdns.CAA{Hdr: hdr(mdns.TypeCAA), Flag: r.Flags, Tag: r.Tag, Value: r.Value}, nil
	}
	return nil, fmt.Errorf("unsupported type %s", r.Type)
}

// splitTXT cuts a value into the 255-byte strings a TXT record is made of.
// The library escapes quotes, backslashes and non-printable bytes in each
// string when packing, so the value goes out exactly as entered.
func splitTXT(v string) []string {
	if v == "" {
		return []string{""}
	}
	var out []string
	for len(v) > 255 {
		out = append(out, escapeTXT(v[:255]))
		v = v[255:]
	}
	return append(out, escapeTXT(v))
}

// escapeTXT turns raw bytes into the presentation form miekg/dns packs
// (it unescapes \" \\ and \DDD); every byte stays one byte on the wire.
func escapeTXT(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c > 0x7e:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// zoneFor finds the most specific zone containing name (lowercase, no
// trailing dot).
func (c *catalog) zoneFor(name string) *zoneData {
	for n := name; n != ""; {
		if z := c.zones[n]; z != nil {
			return z
		}
		_, rest, ok := strings.Cut(n, ".")
		if !ok {
			return nil
		}
		n = rest
	}
	return nil
}
