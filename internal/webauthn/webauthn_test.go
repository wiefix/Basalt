package webauthn

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/webauthn/webauthntest"
)

var rp = RelyingParty{ID: "panel.example.com", Origin: "https://panel.example.com"}

func register(t *testing.T, a *webauthntest.Authenticator) *Credential {
	t.Helper()
	ch := NewChallenge()
	reg := a.Create(rp.ID, rp.Origin, ch, []byte("user-handle"))
	c, err := rp.ParseRegistration(ch, reg.ClientDataJSON, reg.AttestationObject)
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	return c
}

func TestRegisterAndSignIn(t *testing.T) {
	for _, alg := range Algorithms {
		a := webauthntest.New(alg)
		a.Counter = 5
		c := register(t, a)
		if !bytes.Equal(c.ID, a.ID) || c.Alg != alg || c.SignCount != 5 || len(c.AAGUID) != 16 || c.BackupEligible {
			t.Fatalf("alg %d: credential %+v", alg, c)
		}
		ch := NewChallenge()
		as := a.Get(rp.ID, rp.Origin, ch)
		got, err := rp.VerifyAssertion(ch, c, as.ClientDataJSON, as.AuthenticatorData, as.Signature)
		if err != nil || got.SignCount != 6 {
			t.Fatalf("alg %d: sign-in %+v %v", alg, got, err)
		}
	}
}

func TestSyncedPasskey(t *testing.T) {
	a := webauthntest.New(AlgES256)
	a.Flags = flagBE | flagBS
	c := register(t, a)
	if !c.BackupEligible || !c.BackedUp || c.SignCount != 0 {
		t.Fatalf("credential %+v", c)
	}
	// Synced passkeys do not count: 0 stays acceptable.
	for range 2 {
		ch := NewChallenge()
		as := a.Get(rp.ID, rp.Origin, ch)
		if got, err := rp.VerifyAssertion(ch, c, as.ClientDataJSON, as.AuthenticatorData, as.Signature); err != nil || !got.BackedUp {
			t.Fatalf("sign-in: %+v %v", got, err)
		}
	}
}

func TestRegistrationRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(a *webauthntest.Authenticator)
		ch     func(ch []byte) []byte
		mangle func(r *webauthntest.Registration)
		want   string
	}{
		"other challenge":  {ch: func([]byte) []byte { return NewChallenge() }, want: "challenge"},
		"other origin":     {change: func(a *webauthntest.Authenticator) { a.Origin = "https://evil.example" }, want: "evil.example"},
		"other site":       {change: func(a *webauthntest.Authenticator) { a.RPID = "evil.example" }, want: "another site"},
		"no verification":  {change: func(a *webauthntest.Authenticator) { a.NoUV = true }, want: "verify the user"},
		"bad backup flags": {change: func(a *webauthntest.Authenticator) { a.Flags = flagBS }, want: "backup flags"},
		"sign-in data": {mangle: func(r *webauthntest.Registration) {
			r.ClientDataJSON = bytes.Replace(r.ClientDataJSON, []byte("webauthn.create"), []byte("webauthn.get"), 1)
		}, want: "not \"webauthn.create\""},
		"cross origin": {mangle: func(r *webauthntest.Registration) {
			r.ClientDataJSON = bytes.Replace(r.ClientDataJSON, []byte(`"crossOrigin":false`), []byte(`"crossOrigin":true`), 1)
		}, want: "frame"},
		"trailing data": {mangle: func(r *webauthntest.Registration) { r.AttestationObject = append(r.AttestationObject, 0) }, want: "malformed"},
		"truncated": {mangle: func(r *webauthntest.Registration) {
			r.AttestationObject = r.AttestationObject[:len(r.AttestationObject)-3]
		}, want: "CBOR"},
		"garbage json": {mangle: func(r *webauthntest.Registration) { r.ClientDataJSON = []byte("{") }, want: "client data"},
	} {
		a := webauthntest.New(AlgES256)
		if tc.change != nil {
			tc.change(a)
		}
		ch := NewChallenge()
		reg := a.Create(rp.ID, rp.Origin, ch, []byte("u"))
		if tc.mangle != nil {
			tc.mangle(&reg)
		}
		if tc.ch != nil {
			ch = tc.ch(ch)
		}
		_, err := rp.ParseRegistration(ch, reg.ClientDataJSON, reg.AttestationObject)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Keys of other algorithms are refused.
	key := webauthntest.Encode(webauthntest.Map{{K: 1, V: 2}, {K: 3, V: -35}, {K: -1, V: 2}, {K: -2, V: make([]byte, 48)}, {K: -3, V: make([]byte, 48)}})
	if _, err := parseKey(key); err == nil {
		t.Error("ES384 key accepted")
	}
	if _, err := parseKey(webauthntest.Encode(webauthntest.Map{{K: 1, V: 2}, {K: 3, V: -7}, {K: -1, V: 1}, {K: -2, V: make([]byte, 32)}, {K: -3, V: make([]byte, 32)}})); err == nil {
		t.Error("point not on the curve accepted")
	}
}

func TestSignInRefused(t *testing.T) {
	a := webauthntest.New(AlgEdDSA)
	a.Counter = 10
	c := register(t, a)
	other := webauthntest.New(AlgEdDSA)
	other.ID = a.ID
	try := func(au *webauthntest.Authenticator, mangle func(*webauthntest.Assertion), challenge []byte) error {
		ch := NewChallenge()
		as := au.Get(rp.ID, rp.Origin, ch)
		if mangle != nil {
			mangle(&as)
		}
		if challenge != nil {
			ch = challenge
		}
		_, err := rp.VerifyAssertion(ch, c, as.ClientDataJSON, as.AuthenticatorData, as.Signature)
		return err
	}
	if err := try(other, nil, nil); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("another key: %v", err)
	}
	if err := try(a, func(as *webauthntest.Assertion) { as.Signature[0] ^= 1 }, nil); err == nil {
		t.Error("tampered signature accepted")
	}
	if err := try(a, func(as *webauthntest.Assertion) { as.AuthenticatorData[32] |= flagBE | flagBS }, nil); err == nil {
		t.Error("changed flags accepted")
	}
	if err := try(a, nil, NewChallenge()); err == nil {
		t.Error("other challenge accepted")
	}
	// A counter at or below the stored one: a clone.
	c.SignCount = 100
	if err := try(a, nil, nil); !errors.Is(err, ErrCloned) {
		t.Errorf("counter backwards: %v", err)
	}
}

func TestCBOR(t *testing.T) {
	v, n, err := decodeCBOR(webauthntest.Encode(webauthntest.Map{{K: "a", V: []any{1, -2, "x", []byte{1}, true}}, {K: -1, V: webauthntest.Map{}}}))
	if err != nil || n == 0 {
		t.Fatal(err)
	}
	m := v.(map[any]any)
	if l := m["a"].([]any); l[0] != int64(1) || l[1] != int64(-2) || l[2] != "x" || l[4] != true {
		t.Fatalf("decoded %v", m)
	}
	for _, bad := range [][]byte{
		{0xa2, 0x01, 0x01, 0x01, 0x02}, // duplicate key
		{0x5f},                         // indefinite length
		{0x5a, 0xff, 0xff, 0xff, 0xff}, // longer than the data
		{0x9a, 0xff, 0xff, 0xff, 0xff}, // huge array
		{0xa1, 0x40, 0x01},             // byte string key
		{0xfb, 0, 0, 0, 0, 0, 0, 0, 0}, // float
		bytes.Repeat([]byte{0x81}, 20), // nested too deeply
	} {
		if _, _, err := decodeCBOR(bad); err == nil {
			t.Errorf("% x decoded", bad)
		}
	}
}

func FuzzCBOR(f *testing.F) {
	f.Add(webauthntest.New(AlgES256).COSEKey())
	f.Fuzz(func(t *testing.T, b []byte) {
		decodeCBOR(b)
		parseKey(b)
	})
}
