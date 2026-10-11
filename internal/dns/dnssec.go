package dns

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"
)

// DNSSEC: one combined signing key (CSK) per zone, ECDSA P-256 (algorithm
// 13), and online signing: the server signs each answer as it builds it.
// Denial of existence is "compact" (RFC 9824): a missing name gets a signed
// NSEC saying the name exists with no data, so nothing can be enumerated
// and no chain of NSEC records has to be kept.
const (
	dnskeyTTL = 3600
	// Signatures are valid from an hour ago (clock skew) for a week and
	// are renewed when less than three days remain.
	sigBefore  = time.Hour
	sigFor     = 7 * 24 * time.Hour
	sigRenew   = 3 * 24 * time.Hour
	sigCacheSz = 50000
)

// ZoneKey is a zone's signing key. PrivateKey is in BIND's private-key
// format; it travels to the agents, which sign answers with it.
type ZoneKey struct {
	DNSKEY     string `json:"dnskey"` // the DNSKEY record, presentation format
	PrivateKey string `json:"private_key"`
}

// GenerateKey creates a signing key for zone.
func GenerateKey(zone string) (*ZoneKey, error) {
	k := &mdns.DNSKEY{
		Hdr:       mdns.RR_Header{Name: fqdn(zone), Rrtype: mdns.TypeDNSKEY, Class: mdns.ClassINET, Ttl: dnskeyTTL},
		Flags:     mdns.ZONE | mdns.SEP, // 257: a CSK is also the key the DS points to
		Protocol:  3,
		Algorithm: mdns.ECDSAP256SHA256,
	}
	priv, err := k.Generate(256)
	if err != nil {
		return nil, err
	}
	return &ZoneKey{DNSKEY: k.String(), PrivateKey: k.PrivateKeyString(priv)}, nil
}

// parse checks the key belongs to zone and returns its parts.
func (k *ZoneKey) parse(zone string) (*mdns.DNSKEY, crypto.Signer, error) {
	rr, err := mdns.NewRR(k.DNSKEY)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid DNSKEY: %w", err)
	}
	key, ok := rr.(*mdns.DNSKEY)
	if !ok || !strings.EqualFold(key.Hdr.Name, fqdn(zone)) || key.Algorithm != mdns.ECDSAP256SHA256 ||
		key.Flags&mdns.ZONE == 0 || key.Protocol != 3 {
		return nil, nil, errors.New("DNSKEY does not fit the zone")
	}
	priv, err := key.NewPrivateKey(k.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid private key: %w", err)
	}
	ec, ok := priv.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, nil, errors.New("private key is not ECDSA P-256")
	}
	// RFC 6605: the public key is X and Y, 32 bytes each. Derive it from
	// the secret: the parser fills the public half in from the DNSKEY.
	ek, err := ec.ECDH()
	if err != nil {
		return nil, nil, errors.New("invalid private key")
	}
	pub := ek.PublicKey().Bytes() // 0x04 || X || Y
	if base64.StdEncoding.EncodeToString(pub[1:]) != key.PublicKey {
		return nil, nil, errors.New("private key does not match the DNSKEY")
	}
	return key, ec, nil
}

// DSRecords returns the DS record the parent zone (the registrar) needs,
// as SHA-256 digest, in presentation format without owner and TTL.
func (k *ZoneKey) DSRecords(zone string) (keyTag uint16, ds []string, err error) {
	if _, _, err := k.parse(zone); err != nil {
		return 0, nil, err
	}
	keyTag, ds, _, err = PublicKeyInfo(zone, k.DNSKEY)
	return keyTag, ds, err
}

// PublicKeyInfo describes a zone's public key for the registrar: key tag,
// DS records and the DNSKEY data ("257 3 13 <key>").
func PublicKeyInfo(zone, dnskey string) (keyTag uint16, ds []string, rdata string, err error) {
	rr, err := mdns.NewRR(dnskey)
	if err != nil {
		return 0, nil, "", err
	}
	key, ok := rr.(*mdns.DNSKEY)
	if !ok || !strings.EqualFold(key.Hdr.Name, fqdn(zone)) {
		return 0, nil, "", errors.New("DNSKEY does not fit the zone")
	}
	d := key.ToDS(mdns.SHA256)
	ds = []string{fmt.Sprintf("%d %d %d %s", d.KeyTag, d.Algorithm, d.DigestType, strings.ToUpper(d.Digest))}
	return key.KeyTag(), ds, fmt.Sprintf("%d %d %d %s", key.Flags, key.Protocol, key.Algorithm, key.PublicKey), nil
}

// signer signs RRsets of one zone and caches the signatures.
type signer struct {
	zone string
	key  *mdns.DNSKEY
	priv crypto.Signer
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]*mdns.RRSIG
}

func newSigner(zone string, k *ZoneKey, now func() time.Time) (*signer, error) {
	key, priv, err := k.parse(zone)
	if err != nil {
		return nil, err
	}
	return &signer{zone: zone, key: key, priv: priv, now: now, cache: map[string]*mdns.RRSIG{}}, nil
}

// sign returns the RRSIG of an RRset (same owner, type and class).
func (s *signer) sign(rrset []mdns.RR) (mdns.RR, error) {
	var b strings.Builder
	for _, rr := range rrset {
		b.WriteString(strings.ToLower(rr.String()))
		b.WriteByte('\n')
	}
	key := b.String()
	now := s.now()
	s.mu.Lock()
	sig := s.cache[key]
	s.mu.Unlock()
	if sig == nil || time.Unix(int64(sig.Expiration), 0).Sub(now) < sigRenew {
		sig = &mdns.RRSIG{
			Hdr:        mdns.RR_Header{Rrtype: mdns.TypeRRSIG, Class: mdns.ClassINET},
			Algorithm:  s.key.Algorithm,
			KeyTag:     s.key.KeyTag(),
			SignerName: s.key.Hdr.Name,
			Inception:  uint32(now.Add(-sigBefore).Unix()),
			Expiration: uint32(now.Add(sigFor).Unix()),
		}
		if err := sig.Sign(s.priv, rrset); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if len(s.cache) >= sigCacheSz {
			clear(s.cache)
		}
		s.cache[key] = sig
		s.mu.Unlock()
	}
	// The owner as asked: validators compare names in lowercase.
	out := mdns.Copy(sig).(*mdns.RRSIG)
	out.Hdr.Name = rrset[0].Header().Name
	out.Hdr.Ttl = rrset[0].Header().Ttl
	return out, nil
}
