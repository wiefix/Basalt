package dns

import (
	"net/netip"
	"slices"
	"strings"
)

// AutoTTL is the TTL of automatic records: short, so moving a site to
// another server takes effect quickly.
const AutoTTL = 300

// AutoTarget is a domain served by a server reachable at IPs; Source names
// what serves it (a site or an app) for the UI.
type AutoTarget struct {
	Domain string
	IPs    []netip.Addr
	Source string
}

// AutoRecord is a generated record and what it was generated for.
type AutoRecord struct {
	Record
	Source string `json:"source"`
}

// ZoneOf returns the most specific zone among names that contains domain,
// or "".
func ZoneOf(domain string, names []string) string {
	best := ""
	for _, z := range names {
		if (domain == z || strings.HasSuffix(domain, "."+z)) && len(z) > len(best) {
			best = z
		}
	}
	return best
}

// AutoRecords returns the A and AAAA records zone z gets for the targets
// whose domains it holds (zones lists every zone, so a domain goes to the
// most specific one). A name the user gave an address or alias record of
// their own, or that is delegated, gets none: manual records win.
func AutoRecords(z *Zone, zones []string, targets []AutoTarget) []AutoRecord {
	manual := map[string]bool{}
	var cuts []string
	for _, r := range z.Records {
		switch r.Type {
		case TypeA, TypeAAAA, TypeCNAME, TypeALIAS:
			manual[r.Name] = true
		case TypeNS:
			if r.Name != "@" {
				cuts = append(cuts, r.Name)
			}
		}
	}
	type entry struct {
		ips     []netip.Addr
		sources []string
	}
	byName := map[string]*entry{}
	for _, t := range targets {
		if ZoneOf(t.Domain, zones) != z.Name {
			continue
		}
		name := "@"
		if t.Domain != z.Name {
			name = strings.TrimSuffix(t.Domain, "."+z.Name)
		}
		if manual[name] || delegated(name, cuts) {
			continue
		}
		e := byName[name]
		if e == nil {
			e = &entry{}
			byName[name] = e
		}
		for _, ip := range t.IPs {
			if !slices.Contains(e.ips, ip) {
				e.ips = append(e.ips, ip)
			}
		}
		if !slices.Contains(e.sources, t.Source) {
			e.sources = append(e.sources, t.Source)
		}
	}
	var out []AutoRecord
	for name, e := range byName {
		src := strings.Join(e.sources, ", ")
		for _, ip := range e.ips {
			typ := TypeAAAA
			if ip.Is4() {
				typ = TypeA
			}
			out = append(out, AutoRecord{Record: Record{Name: name, Type: typ, TTL: AutoTTL, Value: ip.String()}, Source: src})
		}
	}
	slices.SortFunc(out, func(a, b AutoRecord) int {
		return strings.Compare(a.Name+" "+a.Type+" "+a.Value, b.Name+" "+b.Type+" "+b.Value)
	})
	if room := MaxRecords - len(z.Records); len(out) > room {
		out = out[:max(room, 0)]
	}
	return out
}

// delegated reports whether name is at or below one of the cuts.
func delegated(name string, cuts []string) bool {
	for _, c := range cuts {
		if name == c || strings.HasSuffix(name, "."+c) {
			return true
		}
	}
	return false
}

// PublicAddr reports whether an address can be published for the world:
// global unicast, not private, not shared (CGNAT) space.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

// ChallengeTTL is the TTL of DNS-01 challenge records.
const ChallengeTTL = MinTTL

// Challenge is a DNS-01 challenge record: TXT Value at Name (absolute).
type Challenge struct {
	Name  string
	Value string
}

// ChallengeRecords returns the TXT records zone z serves for challenges in
// it. A name the user pointed elsewhere with a CNAME (delegated
// validation) or that is delegated gets none.
func ChallengeRecords(z *Zone, zones []string, cs []Challenge) []AutoRecord {
	skip := map[string]bool{}
	var cuts []string
	for _, r := range z.Records {
		switch {
		case r.Type == TypeCNAME:
			skip[r.Name] = true
		case r.Type == TypeNS && r.Name != "@":
			cuts = append(cuts, r.Name)
		}
	}
	var out []AutoRecord
	seen := map[string]bool{}
	for _, c := range cs {
		if ZoneOf(c.Name, zones) != z.Name || c.Name == z.Name {
			continue
		}
		name := strings.TrimSuffix(c.Name, "."+z.Name)
		if skip[name] || delegated(name, cuts) || seen[name+" "+c.Value] {
			continue
		}
		seen[name+" "+c.Value] = true
		out = append(out, AutoRecord{Record: Record{Name: name, Type: TypeTXT, TTL: ChallengeTTL, Value: c.Value}, Source: "ACME challenge"})
	}
	return out
}
