// Package dns holds the zone model shared by the panel and the agents, its
// validation, and the authoritative DNS server the agents run.
//
// Zones are stored by the panel and pushed to every agent as structured
// records, never as zone-file text: the server builds wire records from
// validated fields only.
package dns

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Record types users can create. SOA is generated per zone.
const (
	TypeA     = "A"
	TypeAAAA  = "AAAA"
	TypeCNAME = "CNAME"
	// TypeALIAS is a CNAME-like record allowed at the apex: the server
	// answers A/AAAA queries with the target's current addresses.
	TypeALIAS = "ALIAS"
	TypeMX    = "MX"
	TypeTXT   = "TXT"
	TypeSRV   = "SRV"
	TypeCAA   = "CAA"
	TypeNS    = "NS"
	TypePTR   = "PTR"
)

// Types lists the record types in the order the UI offers them.
var Types = []string{TypeA, TypeAAAA, TypeCNAME, TypeALIAS, TypeMX, TypeTXT, TypeSRV, TypeCAA, TypeNS, TypePTR}

// Limits.
const (
	MinTTL         = 60
	MaxTTL         = 7 * 24 * 3600
	DefaultTTL     = 3600
	MaxRecords     = 5000 // per zone
	MaxTXTLength   = 4000
	MaxCAALength   = 255
	MaxNameservers = 8
)

// SOA timers. The serial comes from the panel.
const (
	soaRefresh = 3600
	soaRetry   = 600
	soaExpire  = 1209600
	// NegativeTTL is how long resolvers cache "no such name/record".
	NegativeTTL = 300
	soaTTL      = 3600
)

// Zone is one authoritative zone with all its records.
type Zone struct {
	Name    string   `json:"name"` // "example.com": lowercase, no trailing dot
	Serial  uint32   `json:"serial"`
	Records []Record `json:"records"`
	// DNSSEC, when set, signs the zone with this key.
	DNSSEC *ZoneKey `json:"dnssec,omitempty"`
}

// Record is one resource record. Name is relative to the zone ("@" is the
// apex, "*" or "*.x" a wildcard); host names in Value are absolute.
type Record struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	TTL      uint32 `json:"ttl"`
	Value    string `json:"value"`
	Priority uint16 `json:"priority,omitempty"` // MX, SRV
	Weight   uint16 `json:"weight,omitempty"`   // SRV
	Port     uint16 `json:"port,omitempty"`     // SRV
	Flags    uint8  `json:"flags,omitempty"`    // CAA
	Tag      string `json:"tag,omitempty"`      // CAA
}

var (
	labelRE   = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)
	caaTagRE  = regexp.MustCompile(`^(issue|issuewild|iodef|issuemail|issuevmc)$`)
	errNoName = errors.New("name is empty")
)

// NormalizeZoneName lowercases a zone name and drops a trailing dot.
func NormalizeZoneName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// ValidateZoneName accepts a domain of at least two labels (so a typo
// cannot claim a whole TLD), punycode for internationalized names.
func ValidateZoneName(name string) error {
	if err := validateHost(name); err != nil {
		return fmt.Errorf("invalid zone name: %w", err)
	}
	if !strings.Contains(name, ".") {
		return errors.New("invalid zone name: use a domain such as example.com")
	}
	if strings.Contains(name, "_") {
		return errors.New("invalid zone name: underscores are not allowed")
	}
	return nil
}

// validateHost checks an absolute host name without the trailing dot.
func validateHost(h string) error {
	if h == "" {
		return errNoName
	}
	if len(h) > 253 {
		return errors.New("name is longer than 253 characters")
	}
	for l := range strings.SplitSeq(h, ".") {
		if !labelRE.MatchString(l) {
			if l == "" {
				return errors.New("name has an empty label")
			}
			return fmt.Errorf("label %q may use only a-z, 0-9, - and _ (punycode for other letters), up to 63 characters", l)
		}
	}
	return nil
}

// Normalize lowercases names, drops trailing dots from host names, sets a
// default TTL and clears fields the record type does not use.
func (r *Record) Normalize() {
	r.Type = strings.ToUpper(strings.TrimSpace(r.Type))
	r.Name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.Name)), ".")
	if r.Name == "" {
		r.Name = "@"
	}
	if r.TTL == 0 {
		r.TTL = DefaultTTL
	}
	switch r.Type {
	case TypeTXT, TypeCAA:
	default:
		r.Value = strings.TrimSpace(r.Value)
	}
	switch r.Type {
	case TypeCNAME, TypeALIAS, TypeNS, TypePTR, TypeMX, TypeSRV:
		if r.Value != "." {
			r.Value = strings.TrimSuffix(strings.ToLower(r.Value), ".")
		}
	case TypeA, TypeAAAA:
		if a, err := netip.ParseAddr(r.Value); err == nil {
			r.Value = a.String()
		}
	}
	if r.Type != TypeMX && r.Type != TypeSRV {
		r.Priority = 0
	}
	if r.Type != TypeSRV {
		r.Weight, r.Port = 0, 0
	}
	if r.Type == TypeCAA {
		r.Tag = strings.ToLower(strings.TrimSpace(r.Tag))
	} else {
		r.Flags, r.Tag = 0, ""
	}
}

// FQDN is the record's absolute owner name in zone (without trailing dot).
func (r *Record) FQDN(zone string) string {
	if r.Name == "@" {
		return zone
	}
	return r.Name + "." + zone
}

// Validate checks one normalized record of zone.
func (r *Record) Validate(zone string) error {
	if err := r.validateName(zone); err != nil {
		return err
	}
	if r.TTL < MinTTL || r.TTL > MaxTTL {
		return fmt.Errorf("TTL must be between %d and %d seconds", MinTTL, MaxTTL)
	}
	switch r.Type {
	case TypeA:
		a, err := netip.ParseAddr(r.Value)
		if err != nil || !a.Is4() {
			return errors.New("an A record needs an IPv4 address")
		}
	case TypeAAAA:
		a, err := netip.ParseAddr(r.Value)
		if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
			return errors.New("an AAAA record needs an IPv6 address")
		}
	case TypeCNAME, TypeALIAS, TypeNS, TypePTR:
		if err := validateTarget(r.Value); err != nil {
			return err
		}
		if (r.Type == TypeCNAME || r.Type == TypeALIAS) && r.Value == r.FQDN(zone) {
			return fmt.Errorf("a %s record cannot point to itself", r.Type)
		}
	case TypeMX:
		if r.Value == "." {
			if r.Priority != 0 {
				return errors.New(`a null MX (".") must have priority 0`)
			}
			break
		}
		if err := validateTarget(r.Value); err != nil {
			return err
		}
	case TypeSRV:
		if r.Value != "." {
			if err := validateTarget(r.Value); err != nil {
				return err
			}
		}
		if r.Name == "@" || !strings.HasPrefix(r.Name, "_") {
			return errors.New("an SRV record name looks like _service._proto (e.g. _sip._tcp)")
		}
	case TypeTXT:
		if len(r.Value) > MaxTXTLength {
			return fmt.Errorf("TXT value is longer than %d bytes", MaxTXTLength)
		}
		if err := printable(r.Value, true); err != nil {
			return fmt.Errorf("TXT value %w", err)
		}
	case TypeCAA:
		if r.Flags != 0 && r.Flags != 128 {
			return errors.New("CAA flags must be 0 or 128 (critical)")
		}
		if !caaTagRE.MatchString(r.Tag) {
			return errors.New("CAA tag must be issue, issuewild, iodef, issuemail or issuevmc")
		}
		if len(r.Value) > MaxCAALength {
			return fmt.Errorf("CAA value is longer than %d bytes", MaxCAALength)
		}
		if err := printable(r.Value, false); err != nil {
			return fmt.Errorf("CAA value %w", err)
		}
	case "SOA":
		return errors.New("the SOA record is managed by Basalt")
	default:
		return fmt.Errorf("unsupported record type %q", r.Type)
	}
	return nil
}

func (r *Record) validateName(zone string) error {
	if r.Name == "@" {
		return nil
	}
	name := r.Name
	if name == "*" {
		return nil
	}
	name = strings.TrimPrefix(name, "*.")
	if strings.Contains(name, "*") {
		return errors.New(`a wildcard is allowed only as the leftmost label ("*" or "*.name")`)
	}
	if err := validateHost(name); err != nil {
		return fmt.Errorf("invalid name: %w", err)
	}
	if len(r.FQDN(zone)) > 253 {
		return errors.New("name is too long for this zone")
	}
	return nil
}

func validateTarget(v string) error {
	if v == "." {
		return errors.New(`"." is not a valid target here`)
	}
	if err := validateHost(v); err != nil {
		return fmt.Errorf("invalid target host: %w", err)
	}
	if !strings.Contains(v, ".") {
		return errors.New("target must be a full host name such as host.example.com")
	}
	return nil
}

// printable rejects control characters (and non-ASCII unless allowed).
func printable(s string, utf bool) error {
	if !utf8.ValidString(s) {
		return errors.New("is not valid UTF-8")
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || (!utf && c > 0x7e) {
			return errors.New("contains characters that are not allowed")
		}
	}
	return nil
}

// Validate checks a whole zone: every record, and the rules between them
// (CNAME stands alone, at most one ALIAS per name, no duplicates).
func (z *Zone) Validate() error {
	if err := ValidateZoneName(z.Name); err != nil {
		return err
	}
	if len(z.Records) > MaxRecords {
		return fmt.Errorf("a zone holds at most %d records", MaxRecords)
	}
	if z.DNSSEC != nil {
		if _, _, err := z.DNSSEC.parse(z.Name); err != nil {
			return fmt.Errorf("DNSSEC key: %w", err)
		}
	}
	byName := map[string][]*Record{}
	for i := range z.Records {
		r := &z.Records[i]
		if err := r.Validate(z.Name); err != nil {
			return fmt.Errorf("%s %s: %w", r.Name, r.Type, err)
		}
		byName[r.Name] = append(byName[r.Name], r)
	}
	for name, recs := range byName {
		count := map[string]int{}
		for i, r := range recs {
			count[r.Type]++
			for _, o := range recs[:i] {
				if o.Type == r.Type && sameData(o, r) {
					return fmt.Errorf("%s %s: duplicate record", name, r.Type)
				}
			}
		}
		if count[TypeCNAME] > 0 {
			if name == "@" {
				return errors.New("a CNAME is not allowed at the zone apex: use ALIAS")
			}
			if len(recs) > 1 {
				return fmt.Errorf("%s: a CNAME cannot be combined with other records at the same name", name)
			}
		}
		if count[TypeALIAS] > 1 {
			return fmt.Errorf("%s: only one ALIAS record per name", name)
		}
		if count[TypeALIAS] > 0 && count[TypeA]+count[TypeAAAA] > 0 {
			return fmt.Errorf("%s: an ALIAS cannot be combined with A or AAAA records", name)
		}
	}
	return nil
}

func sameData(a, b *Record) bool {
	return a.Value == b.Value && a.Priority == b.Priority && a.Weight == b.Weight &&
		a.Port == b.Port && a.Flags == b.Flags && a.Tag == b.Tag
}

// Nameservers returns the targets of the apex NS records, in order.
func (z *Zone) Nameservers() []string {
	var out []string
	for _, r := range z.Records {
		if r.Name == "@" && r.Type == TypeNS && !slices.Contains(out, r.Value) {
			out = append(out, r.Value)
		}
	}
	return out
}
