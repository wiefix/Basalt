package auth

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

var fastParams = Argon2Params{Memory: 1024, Time: 1, Threads: 1}

func TestPasswordHashVerify(t *testing.T) {
	ctx := context.Background()
	h := NewHasher(fastParams, 2)
	enc, err := h.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("unexpected encoding %q", enc)
	}
	if ok, rehash, err := h.Verify(ctx, enc, "correct horse battery"); !ok || rehash || err != nil {
		t.Fatalf("Verify correct = %v, %v, %v", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(ctx, enc, "wrong horse battery"); ok {
		t.Fatal("wrong password accepted")
	}

	stronger := NewHasher(Argon2Params{Memory: 2048, Time: 1, Threads: 1}, 1)
	if ok, rehash, _ := stronger.Verify(ctx, enc, "correct horse battery"); !ok || !rehash {
		t.Fatalf("param change: ok=%v rehash=%v; want true, true", ok, rehash)
	}
}

func TestVerifyRejectsMalformedAndHostileHashes(t *testing.T) {
	h := NewHasher(fastParams, 1)
	for _, enc := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=1024,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", // memory bomb
		"$argon2id$v=19$m=1024,t=1,p=0$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if ok, _, err := h.Verify(context.Background(), enc, "x"); ok || err == nil {
			t.Errorf("Verify(%q) = %v, %v; want false, error", enc, ok, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Error("short password accepted")
	}
	if ValidatePassword("пароль-пароль") != nil { // 13 runes, 25 bytes
		t.Error("unicode password rejected")
	}
	if ValidatePassword(strings.Repeat("a", MaxPasswordBytes+1)) == nil {
		t.Error("overlong password accepted")
	}
}

func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	// RFC 6238 Appendix B (SHA1), truncated to 6 digits.
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471",
		1234567890: "005924", 2000000000: "279037", 20000000000: "353130",
	} {
		if got := totpCode(secret, unix/totpPeriod); got != want {
			t.Errorf("t=%d: code %s; want %s", unix, got, want)
		}
	}
}

func TestTOTPValidateWindowAndReplay(t *testing.T) {
	secret, _ := hex.DecodeString("3132333435363738393031323334353637383930")
	now := time.Unix(1111111111, 0)
	step := now.Unix() / totpPeriod
	code := totpCode(secret, step)

	got, ok := ValidateTOTP(secret, code, now, 0)
	if !ok || got != step {
		t.Fatalf("valid code rejected")
	}
	if _, ok := ValidateTOTP(secret, code, now, got); ok {
		t.Fatal("replayed code accepted")
	}
	if _, ok := ValidateTOTP(secret, totpCode(secret, step-1), now, 0); !ok {
		t.Fatal("previous-step code rejected (clock skew)")
	}
	if _, ok := ValidateTOTP(secret, totpCode(secret, step-3), now, 0); ok {
		t.Fatal("stale code accepted")
	}
	if _, ok := ValidateTOTP(secret, "12345", now, 0); ok {
		t.Fatal("short code accepted")
	}
}

func TestTOTPKeyURI(t *testing.T) {
	uri := TOTPKeyURI("Basalt", "a@b.c", []byte("12345678901234567890"))
	if !strings.HasPrefix(uri, "otpauth://totp/Basalt:a@b.c?") || !strings.Contains(uri, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") {
		t.Fatalf("uri = %s", uri)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes := NewRecoveryCodes()
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' || !LooksLikeRecoveryCode(c) || seen[c] {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if NormalizeRecoveryCode(" ABCDE-fghij ") != "abcdefghij" {
		t.Fatal("normalize failed")
	}
	if LooksLikeRecoveryCode("123456") {
		t.Fatal("TOTP code classified as recovery code")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter(3, 10*time.Second)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if !l.Allow("k") {
			t.Fatalf("attempt %d denied within burst", i)
		}
	}
	if l.Allow("k") {
		t.Fatal("burst exceeded but allowed")
	}
	if !l.Allow("other") {
		t.Fatal("keys are not independent")
	}
	now = now.Add(10 * time.Second)
	if !l.Allow("k") {
		t.Fatal("token not refilled after interval")
	}
	if l.Allow("k") {
		t.Fatal("refilled more than one token")
	}
}
