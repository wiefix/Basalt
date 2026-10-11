package store

import (
	"context"
	"errors"
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
	z, err := db.CreateDNSZone(ctx, "example.com", []dns.Record{ns}, check, now)
	if err != nil {
		t.Fatal(err)
	}
	if z.Serial != 2026101100 || z.Records != 1 {
		t.Fatalf("%+v", z)
	}
	if _, err := db.CreateDNSZone(ctx, "example.com", nil, check, now); !errors.Is(err, ErrNameTaken) {
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
	other, err := db.CreateDNSZone(ctx, "example.org", nil, check, now)
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
	z, err := db.CreateDNSZone(ctx, "example.com", nil, func(*dns.Zone) error { return nil }, now)
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
