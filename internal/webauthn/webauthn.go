// Package webauthn verifies passkey registrations and sign-ins (W3C Web
// Authentication, level 3) without third-party code. Only what a relying
// party that asks for no attestation needs: the client data, the
// authenticator data, the credential's public key and assertion
// signatures. Attestation statements are ignored.
package webauthn

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// RelyingParty is the site credentials are bound to: ID is its host name
// (the WebAuthn RP ID), Origin the exact origin requests come from.
type RelyingParty struct {
	ID     string
	Origin string
}

// Credential is a registered passkey.
type Credential struct {
	ID        []byte
	PublicKey []byte // COSE_Key
	Alg       int
	SignCount uint32
	AAGUID    []byte
	// BackupEligible: the passkey can be synced (iCloud Keychain, Google
	// Password Manager…); BackedUp: it is.
	BackupEligible bool
	BackedUp       bool
}

// Authenticator data flags.
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagBE = 0x08 // backup eligible
	flagBS = 0x10 // backed up
	flagAT = 0x40 // attested credential data included
	flagED = 0x80 // extensions included
)

const maxCredentialID = 1023

var (
	ErrCloned = errors.New("the passkey's signature counter went backwards: it may have been cloned")
	b64       = base64.RawURLEncoding
)

// NewChallenge returns a random challenge.
func NewChallenge() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func (rp RelyingParty) checkClientData(raw []byte, typ string, challenge []byte) error {
	var c clientData
	if err := json.Unmarshal(raw, &c); err != nil {
		return errors.New("malformed client data")
	}
	if c.Type != typ {
		return fmt.Errorf("client data is for %q, not %q", c.Type, typ)
	}
	got, err := b64.DecodeString(c.Challenge)
	if err != nil || len(challenge) == 0 || subtle.ConstantTimeCompare(got, challenge) != 1 {
		return errors.New("the challenge does not match (expired or reused)")
	}
	if c.Origin != rp.Origin {
		return fmt.Errorf("the passkey was used on %s, not %s", c.Origin, rp.Origin)
	}
	if c.CrossOrigin {
		return errors.New("passkeys cannot be used from another site's frame")
	}
	return nil
}

type authData struct {
	flags     byte
	signCount uint32
	aaguid    []byte
	credID    []byte
	key       []byte
}

func (rp RelyingParty) parseAuthData(b []byte) (*authData, error) {
	if len(b) < 37 {
		return nil, errors.New("authenticator data too short")
	}
	want := sha256.Sum256([]byte(rp.ID))
	if !bytes.Equal(b[:32], want[:]) {
		return nil, fmt.Errorf("the passkey belongs to another site than %s", rp.ID)
	}
	a := &authData{flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37])}
	if a.flags&flagUP == 0 {
		return nil, errors.New("the user was not present")
	}
	if a.flags&flagUV == 0 {
		return nil, errors.New("the authenticator did not verify the user (PIN or biometrics)")
	}
	if a.flags&flagBS != 0 && a.flags&flagBE == 0 {
		return nil, errors.New("invalid backup flags")
	}
	rest := b[37:]
	if a.flags&flagAT != 0 {
		if len(rest) < 18 {
			return nil, errors.New("attested credential data too short")
		}
		a.aaguid = append([]byte(nil), rest[:16]...)
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		rest = rest[18:]
		if n == 0 || n > maxCredentialID || n > len(rest) {
			return nil, errors.New("invalid credential ID")
		}
		a.credID = append([]byte(nil), rest[:n]...)
		rest = rest[n:]
		_, used, err := decodeCBOR(rest)
		if err != nil {
			return nil, fmt.Errorf("credential public key: %w", err)
		}
		a.key = append([]byte(nil), rest[:used]...)
		rest = rest[used:]
	}
	if a.flags&flagED != 0 {
		_, used, err := decodeCBOR(rest)
		if err != nil {
			return nil, fmt.Errorf("extensions: %w", err)
		}
		rest = rest[used:]
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing data in the authenticator data")
	}
	return a, nil
}

// ParseRegistration verifies a new credential (navigator.credentials.create
// with attestation "none") against the challenge it answers.
func (rp RelyingParty) ParseRegistration(challenge, clientDataJSON, attestationObject []byte) (*Credential, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.create", challenge); err != nil {
		return nil, err
	}
	v, n, err := decodeCBOR(attestationObject)
	if err != nil {
		return nil, fmt.Errorf("attestation object: %w", err)
	}
	obj, ok := v.(map[any]any)
	if !ok || n != len(attestationObject) {
		return nil, errors.New("malformed attestation object")
	}
	if _, ok := obj["fmt"].(string); !ok {
		return nil, errors.New("attestation object without a format")
	}
	raw, ok := obj["authData"].([]byte)
	if !ok {
		return nil, errors.New("attestation object without authenticator data")
	}
	a, err := rp.parseAuthData(raw)
	if err != nil {
		return nil, err
	}
	if a.credID == nil {
		return nil, errors.New("no credential in the authenticator data")
	}
	key, err := parseKey(a.key)
	if err != nil {
		return nil, err
	}
	return &Credential{ID: a.credID, PublicKey: a.key, Alg: key.alg, SignCount: a.signCount, AAGUID: a.aaguid,
		BackupEligible: a.flags&flagBE != 0, BackedUp: a.flags&flagBS != 0}, nil
}

// Assertion is the outcome of a verified sign-in.
type Assertion struct {
	SignCount uint32
	BackedUp  bool
}

// VerifyAssertion checks a sign-in (navigator.credentials.get) with cred
// against the challenge it answers.
func (rp RelyingParty) VerifyAssertion(challenge []byte, cred *Credential, clientDataJSON, authenticatorData, signature []byte) (Assertion, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.get", challenge); err != nil {
		return Assertion{}, err
	}
	a, err := rp.parseAuthData(authenticatorData)
	if err != nil {
		return Assertion{}, err
	}
	if a.credID != nil {
		return Assertion{}, errors.New("unexpected credential data in a sign-in")
	}
	key, err := parseKey(cred.PublicKey)
	if err != nil {
		return Assertion{}, err
	}
	hash := sha256.Sum256(clientDataJSON)
	if !key.verify(append(append([]byte(nil), authenticatorData...), hash[:]...), signature) {
		return Assertion{}, errors.New("invalid signature")
	}
	// Authenticators that count (most synced passkeys always send 0) must
	// count up.
	if (a.signCount != 0 || cred.SignCount != 0) && a.signCount <= cred.SignCount {
		return Assertion{}, ErrCloned
	}
	return Assertion{SignCount: a.signCount, BackedUp: a.flags&flagBS != 0}, nil
}
