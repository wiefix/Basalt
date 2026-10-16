// Package webauthntest is a software authenticator for tests: it makes
// passkeys and signs in with them as a browser and security key would.
package webauthntest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
)

// Authenticator holds one passkey.
type Authenticator struct {
	Alg        int // -7 ES256, -8 EdDSA, -257 RS256
	ID         []byte
	UserHandle []byte
	Counter    uint32 // 0: does not count (like synced passkeys)
	Flags      byte   // extra flags (BE 0x08, BS 0x10)
	// Origin and RPID override those of the browser (attacks in tests).
	Origin string
	RPID   string
	NoUV   bool // the user was not verified

	ec  *ecdsa.PrivateKey
	ed  ed25519.PrivateKey
	rsa *rsa.PrivateKey
}

// New makes an authenticator with a fresh key of alg.
func New(alg int) *Authenticator {
	a := &Authenticator{Alg: alg, ID: make([]byte, 16)}
	rand.Read(a.ID)
	switch alg {
	case -7:
		a.ec, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case -8:
		_, a.ed, _ = ed25519.GenerateKey(rand.Reader)
	case -257:
		a.rsa, _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	return a
}

// Registration is what a browser sends after navigator.credentials.create.
type Registration struct {
	ClientDataJSON    []byte
	AttestationObject []byte
}

// Assertion is what a browser sends after navigator.credentials.get.
type Assertion struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
	UserHandle        []byte
}

func (a *Authenticator) clientData(typ, origin string, challenge []byte) []byte {
	if a.Origin != "" {
		origin = a.Origin
	}
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"origin": origin, "crossOrigin": false})
	return b
}

func (a *Authenticator) authData(rpID string, attested bool) []byte {
	if a.RPID != "" {
		rpID = a.RPID
	}
	h := sha256.Sum256([]byte(rpID))
	flags := byte(0x01|0x04) | a.Flags
	if a.NoUV {
		flags &^= 0x04
	}
	if attested {
		flags |= 0x40
	}
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, a.Counter)
	if attested {
		out = append(out, make([]byte, 16)...) // AAGUID
		out = binary.BigEndian.AppendUint16(out, uint16(len(a.ID)))
		out = append(out, a.ID...)
		out = append(out, a.COSEKey()...)
	}
	return out
}

// COSEKey is the public key as COSE_Key.
func (a *Authenticator) COSEKey() []byte {
	switch a.Alg {
	case -7:
		raw, _ := a.ec.PublicKey.Bytes()
		return Encode(Map{{1, 2}, {3, -7}, {-1, 1}, {-2, raw[1:33]}, {-3, raw[33:]}})
	case -8:
		return Encode(Map{{1, 1}, {3, -8}, {-1, 6}, {-2, []byte(a.ed.Public().(ed25519.PublicKey))}})
	default:
		return Encode(Map{{1, 3}, {3, -257}, {-1, a.rsa.N.Bytes()}, {-2, big.NewInt(int64(a.rsa.E)).Bytes()}})
	}
}

// Create registers the passkey for user on rpID, answering challenge.
func (a *Authenticator) Create(rpID, origin string, challenge, user []byte) Registration {
	a.UserHandle = user
	att := Encode(Map{{"fmt", "none"}, {"attStmt", Map{}}, {"authData", a.authData(rpID, true)}})
	return Registration{ClientDataJSON: a.clientData("webauthn.create", origin, challenge), AttestationObject: att}
}

// Get signs in, answering challenge.
func (a *Authenticator) Get(rpID, origin string, challenge []byte) Assertion {
	if a.Counter > 0 {
		a.Counter++
	}
	cd := a.clientData("webauthn.get", origin, challenge)
	ad := a.authData(rpID, false)
	h := sha256.Sum256(cd)
	msg := append(append([]byte(nil), ad...), h[:]...)
	return Assertion{CredentialID: a.ID, ClientDataJSON: cd, AuthenticatorData: ad, Signature: a.sign(msg), UserHandle: a.UserHandle}
}

func (a *Authenticator) sign(msg []byte) []byte {
	switch a.Alg {
	case -7:
		h := sha256.Sum256(msg)
		sig, _ := ecdsa.SignASN1(rand.Reader, a.ec, h[:])
		return sig
	case -8:
		return ed25519.Sign(a.ed, msg)
	default:
		h := sha256.Sum256(msg)
		sig, _ := rsa.SignPKCS1v15(rand.Reader, a.rsa, crypto.SHA256, h[:])
		return sig
	}
}

// Map is a CBOR map with its keys in order.
type Map []KV

type KV struct {
	K, V any
}

// Encode encodes v (int, string, []byte, bool, Map, []any) as CBOR.
func Encode(v any) []byte {
	head := func(major byte, n uint64) []byte {
		switch {
		case n < 24:
			return []byte{major<<5 | byte(n)}
		case n < 1<<8:
			return []byte{major<<5 | 24, byte(n)}
		case n < 1<<16:
			return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(n))
		case n < 1<<32:
			return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(n))
		}
		return binary.BigEndian.AppendUint64([]byte{major<<5 | 27}, n)
	}
	switch x := v.(type) {
	case int:
		if x >= 0 {
			return head(0, uint64(x))
		}
		return head(1, uint64(-1-x))
	case string:
		return append(head(3, uint64(len(x))), x...)
	case []byte:
		return append(head(2, uint64(len(x))), x...)
	case bool:
		if x {
			return []byte{0xf5}
		}
		return []byte{0xf4}
	case Map:
		out := head(5, uint64(len(x)))
		for _, kv := range x {
			out = append(out, Encode(kv.K)...)
			out = append(out, Encode(kv.V)...)
		}
		return out
	case []any:
		out := head(4, uint64(len(x)))
		for _, e := range x {
			out = append(out, Encode(e)...)
		}
		return out
	}
	panic("webauthntest: cannot encode value")
}
