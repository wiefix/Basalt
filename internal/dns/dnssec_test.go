package dns

import (
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func signedServer(t *testing.T) (*Server, *mdns.DNSKEY, *mdns.DNSKEY) {
	t.Helper()
	zones := testZones()
	k, err := GenerateKey("example.com")
	if err != nil {
		t.Fatal(err)
	}
	ck, err := GenerateKey("child.example.com")
	if err != nil {
		t.Fatal(err)
	}
	zones[0].DNSSEC, zones[1].DNSSEC = k, ck
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeResolver{})
	if err := s.SetZones(zones); err != nil {
		t.Fatal(err)
	}
	key, _, _ := k.parse("example.com")
	ckey, _, _ := ck.parse("child.example.com")
	return s, key, ckey
}

func queryDO(s *Server, name string, qtype uint16) *mdns.Msg {
	m := new(mdns.Msg)
	m.SetQuestion(name, qtype)
	m.SetEdns0(1232, true)
	return s.answer(m)
}

// verifySection checks that every RRset in rrs, except those listed in
// unsigned, carries a valid signature by key, and returns the set types.
func verifySection(t *testing.T, label string, rrs []mdns.RR, keys map[string]*mdns.DNSKEY, unsigned ...uint16) []uint16 {
	t.Helper()
	type set struct {
		rrs []mdns.RR
		sig *mdns.RRSIG
	}
	var order []string
	sets := map[string]*set{}
	for _, rr := range rrs {
		h := rr.Header()
		if sig, ok := rr.(*mdns.RRSIG); ok {
			k := strings.ToLower(h.Name) + "/" + mdns.TypeToString[sig.TypeCovered]
			if sets[k] == nil {
				t.Fatalf("%s: RRSIG before its set: %v", label, rr)
			}
			sets[k].sig = sig
			continue
		}
		k := strings.ToLower(h.Name) + "/" + mdns.TypeToString[h.Rrtype]
		if sets[k] == nil {
			sets[k] = &set{}
			order = append(order, k)
		}
		sets[k].rrs = append(sets[k].rrs, rr)
	}
	var types []uint16
	for _, k := range order {
		st := sets[k]
		typ := st.rrs[0].Header().Rrtype
		types = append(types, typ)
		skip := false
		for _, u := range unsigned {
			skip = skip || u == typ
		}
		if skip {
			if st.sig != nil {
				t.Errorf("%s: %s should not be signed", label, k)
			}
			continue
		}
		if st.sig == nil {
			t.Errorf("%s: %s not signed", label, k)
			continue
		}
		key := keys[strings.ToLower(st.sig.SignerName)]
		if key == nil {
			t.Errorf("%s: %s signed by unknown %s", label, k, st.sig.SignerName)
			continue
		}
		if err := st.sig.Verify(key, st.rrs); err != nil {
			t.Errorf("%s: %s: %v", label, k, err)
		}
		if !st.sig.ValidityPeriod(time.Now()) {
			t.Errorf("%s: %s: signature not valid now", label, k)
		}
		if st.sig.Labels != uint8(mdns.CountLabel(st.rrs[0].Header().Name)) {
			t.Errorf("%s: %s: labels %d", label, k, st.sig.Labels)
		}
	}
	return types
}

func TestDNSSEC(t *testing.T) {
	s, key, ckey := signedServer(t)
	keys := map[string]*mdns.DNSKEY{"example.com.": key, "child.example.com.": ckey}

	for _, tc := range []struct {
		name  string
		qtype uint16
	}{
		{"example.com.", mdns.TypeA}, {"Example.COM.", mdns.TypeMX}, {"example.com.", mdns.TypeTXT},
		{"example.com.", mdns.TypeNS}, {"example.com.", mdns.TypeSOA}, {"example.com.", mdns.TypeDNSKEY},
		{"example.com.", mdns.TypeCDS}, {"example.com.", mdns.TypeCDNSKEY},
		{"www.example.com.", mdns.TypeA}, // CNAME + A, both signed
		{"x.wild.example.com.", mdns.TypeA},
		{"app.example.com.", mdns.TypeA}, // ALIAS, synthesized and signed
		{"outside.example.com.", mdns.TypeA},
		{"child.example.com.", mdns.TypeA},
	} {
		m := queryDO(s, tc.name, tc.qtype)
		label := tc.name + " " + mdns.TypeToString[tc.qtype]
		if m.Rcode != mdns.RcodeSuccess || len(m.Answer) == 0 || !m.IsEdns0().Do() {
			t.Fatalf("%s: %v", label, m)
		}
		verifySection(t, label, m.Answer, keys)
	}

	// The DNSKEY is the one the DS points to.
	m := queryDO(s, "example.com.", mdns.TypeDNSKEY)
	if k := m.Answer[0].(*mdns.DNSKEY); k.Flags != 257 || k.Algorithm != mdns.ECDSAP256SHA256 || k.KeyTag() != key.KeyTag() {
		t.Fatalf("DNSKEY %v", k)
	}

	// NODATA: SOA and an NSEC listing what the name has.
	m = queryDO(s, "example.com.", mdns.TypePTR)
	types := verifySection(t, "nodata", m.Ns, keys)
	nsec := findNSEC(m.Ns)
	if m.Rcode != mdns.RcodeSuccess || len(types) != 2 || nsec == nil || nsec.NextDomain != `\000.example.com.` ||
		!hasTypes(nsec, mdns.TypeA, mdns.TypeMX, mdns.TypeSOA, mdns.TypeNS, mdns.TypeDNSKEY, mdns.TypeRRSIG, mdns.TypeNSEC) ||
		hasTypes(nsec, mdns.TypePTR) {
		t.Fatalf("nodata: %v", m)
	}
	// A name that does not exist: NOERROR with NXNAME (compact denial).
	m = queryDO(s, "nope.example.com.", mdns.TypeA)
	verifySection(t, "nxname", m.Ns, keys)
	nsec = findNSEC(m.Ns)
	if m.Rcode != mdns.RcodeSuccess || nsec == nil || !hasTypes(nsec, mdns.TypeNXNAME) || hasTypes(nsec, mdns.TypeA) ||
		nsec.Hdr.Name != "nope.example.com." {
		t.Fatalf("nxname: %v", m)
	}
	// Without DO nothing is signed and NXDOMAIN stays.
	if m := query(s, "nope.example.com.", mdns.TypeA); m.Rcode != mdns.RcodeNameError || findNSEC(m.Ns) != nil {
		t.Fatalf("plain nxdomain: %v", m)
	}
	if m := query(s, "example.com.", mdns.TypeA); len(m.Answer) != 1 {
		t.Fatalf("plain answer has signatures: %v", m.Answer)
	}

	// A delegation: unsigned NS, signed proof that there is no DS.
	m = queryDO(s, "host.sub.example.com.", mdns.TypeA)
	verifySection(t, "referral", m.Ns, keys, mdns.TypeNS)
	if nsec := findNSEC(m.Ns); m.Authoritative || nsec == nil || nsec.Hdr.Name != "sub.example.com." ||
		!hasTypes(nsec, mdns.TypeNS) || hasTypes(nsec, mdns.TypeDS) {
		t.Fatalf("referral: %v", m)
	}

	// The DS of a child zone we also serve comes from the parent, signed by it.
	m = queryDO(s, "child.example.com.", mdns.TypeDS)
	verifySection(t, "child DS", m.Answer, keys)
	ds, ok := m.Answer[0].(*mdns.DS)
	if !ok || ds.KeyTag != ckey.KeyTag() || m.Answer[1].(*mdns.RRSIG).SignerName != "example.com." {
		t.Fatalf("child DS: %v", m)
	}
	if want := ckey.ToDS(mdns.SHA256); !strings.EqualFold(ds.Digest, want.Digest) {
		t.Fatal("child DS digest")
	}

	// Signatures are reused, not made for every query.
	a := queryDO(s, "example.com.", mdns.TypeA).Answer[1].(*mdns.RRSIG)
	b := queryDO(s, "EXAMPLE.com.", mdns.TypeA).Answer[1].(*mdns.RRSIG)
	if a.Signature != b.Signature {
		t.Fatal("signature not cached")
	}
}

func TestSignatureRenewal(t *testing.T) {
	k, _ := GenerateKey("example.com")
	now := time.Now()
	sg, err := newSigner("example.com", k, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	set := []mdns.RR{&mdns.A{Hdr: mdns.RR_Header{Name: "example.com.", Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 1}}}
	first, _ := sg.sign(set)
	now = now.Add(3 * 24 * time.Hour)
	same, _ := sg.sign(set)
	now = now.Add(2 * 24 * time.Hour) // under three days left
	renewed, _ := sg.sign(set)
	if first.(*mdns.RRSIG).Signature != same.(*mdns.RRSIG).Signature || renewed.(*mdns.RRSIG).Expiration <= first.(*mdns.RRSIG).Expiration {
		t.Fatal("renewal")
	}
	if !renewed.(*mdns.RRSIG).ValidityPeriod(now) {
		t.Fatal("renewed signature not valid")
	}
}

func TestZoneKey(t *testing.T) {
	k, err := GenerateKey("example.com")
	if err != nil {
		t.Fatal(err)
	}
	tag, ds, err := k.DSRecords("example.com")
	if err != nil || len(ds) != 1 || !strings.HasPrefix(ds[0], strconv.Itoa(int(tag))+" 13 2 ") || len(ds[0]) < 64 {
		t.Fatalf("DS %d %v %v", tag, ds, err)
	}
	other, _ := GenerateKey("example.com")
	for _, bad := range []*ZoneKey{
		{DNSKEY: k.DNSKEY, PrivateKey: other.PrivateKey}, // mismatched pair
		{DNSKEY: strings.Replace(k.DNSKEY, "example.com.", "example.org.", 1), PrivateKey: k.PrivateKey},
		{DNSKEY: "garbage", PrivateKey: k.PrivateKey},
		{DNSKEY: k.DNSKEY, PrivateKey: "garbage"},
	} {
		z := Zone{Name: "example.com", DNSSEC: bad}
		if z.Validate() == nil {
			t.Errorf("accepted key %+v", bad)
		}
	}
}

func findNSEC(rrs []mdns.RR) *mdns.NSEC {
	for _, rr := range rrs {
		if n, ok := rr.(*mdns.NSEC); ok {
			return n
		}
	}
	return nil
}

func hasTypes(n *mdns.NSEC, types ...uint16) bool {
	for _, t := range types {
		found := false
		for _, b := range n.TypeBitMap {
			found = found || b == t
		}
		if !found {
			return false
		}
	}
	return true
}
