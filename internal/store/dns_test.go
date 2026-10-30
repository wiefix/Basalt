package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/dns"
)

func TestDNSZones(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	check := func(z *dns.Zone) error { return z.Validate() }
	ns := dns.Record{Name: "@", Type: dns.TypeNS, Value: "ns1.example.net"}
	ns.Normalize()
	z, err := db.CreateDNSZone(ctx, 0, "example.com", []dns.Record{ns}, check, now)
	if err != nil {
		t.Fatal(err)
	}
	if z.Serial != 2026101100 || z.Records != 1 {
		t.Fatalf("%+v", z)
	}
	if _, err := db.CreateDNSZone(ctx, 0, "example.com", nil, check, now); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate zone: %v", err)
	}
	rev0, _, _ := db.DesiredDNS(ctx, nil)

	a := dns.Record{Name: "www", Type: dns.TypeA, Value: "192.0.2.1"}
	a.Normalize()
	ra, err := db.PutDNSRecord(ctx, z.ID, 0, &a, check, now)
	if err != nil {
		t.Fatal(err)
	}
	// The check sees the zone after the change: a CNAME next to the A is refused.
	c := dns.Record{Name: "www", Type: dns.TypeCNAME, Value: "example.org"}
	c.Normalize()
	if _, err := db.PutDNSRecord(ctx, z.ID, 0, &c, check, now); err == nil {
		t.Fatal("CNAME next to A accepted")
	}
	// Replacing the A by the CNAME is fine.
	if _, err := db.PutDNSRecord(ctx, z.ID, ra.ID, &c, check, now); err != nil {
		t.Fatal(err)
	}
	// Records of another zone are not reachable.
	other, err := db.CreateDNSZone(ctx, 0, "example.org", nil, check, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutDNSRecord(ctx, other.ID, ra.ID, nil, check, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-zone delete: %v", err)
	}

	rev, zones, err := db.DesiredDNS(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rev <= rev0 || len(zones) != 2 || zones[0].Name != "example.com" || len(zones[0].Records) != 2 {
		t.Fatalf("rev %d->%d zones %+v", rev0, rev, zones)
	}
	// Two changes on the same day: the serial still grows.
	if zones[0].Serial != 2026101102 {
		t.Fatalf("serial %d", zones[0].Serial)
	}
	if err := dns.Check(zones); err != nil {
		t.Fatal(err)
	}

	if _, err := db.PutDNSRecord(ctx, z.ID, ra.ID, nil, check, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := db.DNSZone(ctx, z.ID)
	if got.Serial != 2026101300 || got.Records != 1 {
		t.Fatalf("after delete: %+v", got)
	}
	if err := db.DeleteDNSZone(ctx, z.ID); err != nil {
		t.Fatal(err)
	}
	if recs, _ := db.DNSRecords(ctx, z.ID); len(recs) != 0 {
		t.Fatal("records not deleted with the zone")
	}
	if err := db.DeleteDNSZone(ctx, z.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	if hosts, err := db.DNSNameservers(ctx); err != nil || len(hosts) != 0 {
		t.Fatal(hosts, err)
	}
	if err := db.SetDNSNameservers(ctx, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatal(err)
	}
	if hosts, _ := db.DNSNameservers(ctx); len(hosts) != 2 {
		t.Fatal(hosts)
	}
}

func TestZoneDNSSEC(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	z, err := db.CreateDNSZone(ctx, 0, "example.com", nil, func(*dns.Zone) error { return nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := dns.GenerateKey("example.com")
	if err := db.SetZoneDNSSEC(ctx, z.ID, k.DNSKEY, []byte("sealed:"+k.PrivateKey), now); err != nil {
		t.Fatal(err)
	}
	unseal := func(id int64, sealed []byte) (string, error) {
		if id != z.ID {
			t.Fatalf("unseal for zone %d", id)
		}
		return string(sealed[len("sealed:"):]), nil
	}
	_, zones, err := db.DesiredDNS(ctx, unseal)
	if err != nil || zones[0].DNSSEC == nil || zones[0].DNSSEC.PrivateKey != k.PrivateKey || dns.Check(zones) != nil {
		t.Fatalf("%+v %v", zones, err)
	}
	// Without unseal no key leaves the store (overviews, automatic records).
	if _, zones, _ := db.DesiredDNS(ctx, nil); zones[0].DNSSEC != nil {
		t.Fatal("key without unseal")
	}
	got, _ := db.DNSZone(ctx, z.ID)
	if got.DNSKEY != k.DNSKEY || got.Serial != 2026101101 {
		t.Fatalf("%+v", got)
	}
	if err := db.SetZoneDNSSEC(ctx, z.ID, "", []byte("x"), now); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	db.R.QueryRow(`SELECT dnssec_key FROM dns_zones WHERE id = ?`, z.ID).Scan(&sealed)
	if got, _ := db.DNSZone(ctx, z.ID); got.DNSKEY != "" || sealed != nil {
		t.Fatal("key kept after disabling")
	}
}

func TestDNSFailover(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	check := func(z *dns.Zone) error { return z.Validate() }
	z, err := db.CreateDNSZone(ctx, 0, "example.com", nil, check, now)
	if err != nil {
		t.Fatal(err)
	}
	hc := func(name string) *HealthCheck {
		c := &HealthCheck{Name: name, Kind: "tcp", Host: "192.0.2.1", Port: 80, Interval: 30, Timeout: 5}
		if err := db.CreateHealthCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	one, two, unused := hc("one"), hc("two"), hc("unused")
	put := func(value string, check int64) (*DNSRecord, error) {
		r := dns.Record{Name: "www", Type: dns.TypeA, Value: value, HealthCheck: check}
		r.Normalize()
		return db.PutDNSRecord(ctx, z.ID, 0, &r, func(z *dns.Zone) error { return z.Validate() }, now)
	}
	if _, err := put("192.0.2.9", 999); !errors.Is(err, ErrNoHealthCheck) {
		t.Fatalf("unknown check: %v", err)
	}
	r1, err := put("192.0.2.1", one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put("192.0.2.2", two.ID); err != nil {
		t.Fatal(err)
	}
	if recs, _ := db.DNSRecords(ctx, z.ID); len(recs) != 2 || recs[0].HealthCheck != one.ID || recs[0].TTL != dns.FailoverTTL {
		t.Fatalf("records: %+v", recs)
	}
	served := func() string {
		_, zones, err := db.DesiredDNS(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range zones[0].Records {
			out = append(out, r.Value)
		}
		return strings.Join(out, " ")
	}
	revision := func() int64 { rev, _, _ := db.DesiredDNS(ctx, nil); return rev }

	rev := revision()
	if d, err := db.SetHealthStatus(ctx, one.ID, HealthDown, "x", now); err != nil || !d {
		t.Fatalf("set status: %v %v", d, err)
	}
	if got := served(); got != "192.0.2.2" || revision() <= rev {
		t.Fatalf("one down: %q, revision %d -> %d", got, rev, revision())
	}
	// Both down: every address is served rather than none.
	db.SetHealthStatus(ctx, two.ID, HealthDown, "x", now)
	if got := served(); got != "192.0.2.1 192.0.2.2" {
		t.Fatalf("both down: %q", got)
	}
	db.SetHealthStatus(ctx, two.ID, HealthUp, "x", now)
	if got := served(); got != "192.0.2.2" {
		t.Fatalf("two up: %q", got)
	}
	// A check no record follows does not touch DNS.
	rev = revision()
	if d, err := db.SetHealthStatus(ctx, unused.ID, HealthDown, "x", now); err != nil || d || revision() != rev {
		t.Fatalf("unused check: %v %v", d, err)
	}
	// Editing a check starts its status over: its address is back.
	one.Name = "one-edited"
	if d, err := db.UpdateHealthCheck(ctx, one); err != nil || !d {
		t.Fatalf("update: %v %v", d, err)
	}
	if got := served(); got != "192.0.2.1 192.0.2.2" {
		t.Fatalf("after edit: %q", got)
	}
	// Deleting a check keeps its records, no longer following anything.
	db.SetHealthStatus(ctx, one.ID, HealthDown, "x", now)
	if d, err := db.DeleteHealthCheck(ctx, one.ID); err != nil || !d {
		t.Fatalf("delete: %v %v", d, err)
	}
	recs, _ := db.DNSRecords(ctx, z.ID)
	if len(recs) != 2 || recs[0].ID != r1.ID || recs[0].HealthCheck != 0 {
		t.Fatalf("after delete: %+v", recs)
	}
	if got := served(); got != "192.0.2.1 192.0.2.2" {
		t.Fatalf("after delete: %q", got)
	}
	if d, err := db.DeleteHealthCheck(ctx, unused.ID); err != nil || d {
		t.Fatalf("delete unused: %v %v", d, err)
	}
}

// TestPanelDomainDNS: the panel's domain, in a zone of Basalt's DNS, gets
// the addresses of the server the panel runs on, and follows it to a
// standby's server when that one takes over.
func TestPanelDomainDNS(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	z, err := db.CreateDNSZone(ctx, 0, "example.com", nil, func(z *dns.Zone) error { return z.Validate() }, now)
	if err != nil {
		t.Fatal(err)
	}
	server := func(name, ip string) int64 {
		srv, err := db.CreateServer(ctx, name, []byte(name), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		db.W.Exec(`UPDATE servers SET enrolled_at = 1, public_ips = ? WHERE id = ?`, `["`+ip+`"]`, srv.ID)
		return srv.ID
	}
	main, standby := server("main", "192.0.2.10"), server("standby", "192.0.2.20")
	panel := func() string {
		t.Helper()
		recs, err := db.DNSZoneAuto(ctx, z.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range recs {
			if r.Source == "panel" {
				out = append(out, r.Name+" "+r.Value)
			}
		}
		return strings.Join(out, ", ")
	}
	if got := panel(); got != "" {
		t.Fatalf("without a panel address: %q", got)
	}
	db.SetSetting(ctx, "panel_url", "https://Panel.example.com")
	if got := panel(); got != "" {
		t.Fatalf("without the panel's server: %q", got)
	}
	db.SetSetting(ctx, SettingPanelServer, strconv.FormatInt(main, 10))
	if got := panel(); got != "panel 192.0.2.10" {
		t.Fatalf("panel domain: %q", got)
	}
	// A standby took over: its host's server is the panel's now.
	db.SetSetting(ctx, SettingPanelServer, strconv.FormatInt(standby, 10))
	if got := panel(); got != "panel 192.0.2.20" {
		t.Fatalf("after a takeover: %q", got)
	}
	if changed, err := db.SyncAutoDNS(ctx, now); err != nil || !changed {
		t.Fatalf("sync: %v %v", changed, err)
	}
	// An address (no domain), or a domain outside the zones: nothing.
	db.SetSetting(ctx, "panel_url", "https://192.0.2.20:8443")
	if got := panel(); got != "" {
		t.Fatalf("panel at an address: %q", got)
	}
	db.SetSetting(ctx, "panel_url", "https://panel.example.org")
	if got := panel(); got != "" {
		t.Fatalf("panel outside the zones: %q", got)
	}
}
