package mailserver

import (
	"strings"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/dns"
)

// recordTTL is the TTL of the mail records Basalt publishes.
const recordTTL = 3600

// ParseZone converts the DNS records the mail server asks for (its zone
// file for a domain) into records with absolute names. Types Basalt does
// not publish for mail are skipped, as are records that do not validate.
func ParseZone(zone string) []dns.Record {
	var out []dns.Record
	zp := mdns.NewZoneParser(strings.NewReader(zone), ".", "")
	zp.SetDefaultTTL(recordTTL)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		name := strings.TrimSuffix(strings.ToLower(rr.Header().Name), ".")
		r := dns.Record{Name: name, TTL: recordTTL}
		switch v := rr.(type) {
		case *mdns.MX:
			r.Type, r.Priority, r.Value = dns.TypeMX, v.Preference, v.Mx
		case *mdns.TXT:
			r.Type, r.Value = dns.TypeTXT, unescapeTXT(strings.Join(v.Txt, ""))
		case *mdns.SRV:
			r.Type, r.Priority, r.Weight, r.Port, r.Value = dns.TypeSRV, v.Priority, v.Weight, v.Port, v.Target
		case *mdns.CNAME:
			r.Type, r.Value = dns.TypeCNAME, v.Target
		default:
			continue
		}
		r.Normalize()
		r.Name = name // Normalize treats names as relative; keep it absolute
		if r.TTL != recordTTL || name == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// unescapeTXT undoes the presentation escapes miekg/dns keeps in TXT
// strings (\" and \\).
func unescapeTXT(s string) string {
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s)
}
