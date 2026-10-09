package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP per RFC 6238 with the parameters every authenticator app supports:
// HMAC-SHA1, 6 digits, 30-second steps.
const (
	totpPeriod    = 30
	totpDigits    = 6
	totpSecretLen = 20
	totpSkew      = 1 // accept one step before and after to tolerate clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() []byte {
	s := make([]byte, totpSecretLen)
	rand.Read(s)
	return s
}

// TOTPKeyURI returns the otpauth:// URI that authenticator apps import via QR code.
func TOTPKeyURI(issuer, account string, secret []byte) string {
	v := url.Values{}
	v.Set("secret", b32.EncodeToString(secret))
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(totpDigits))
	v.Set("period", fmt.Sprint(totpPeriod))
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// TOTPSecretString is the base32 form shown to users who type the secret manually.
func TOTPSecretString(secret []byte) string { return b32.EncodeToString(secret) }

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, n%1_000_000)
}

// ValidateTOTP checks code against the steps around now. A code is accepted
// only for a step strictly greater than lastStep, so each code works once.
// It returns the matched step, which the caller must persist as the new lastStep.
func ValidateTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	var matched int64
	ok := 0
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		eq := subtle.ConstantTimeCompare([]byte(totpCode(secret, step)), []byte(code))
		if eq == 1 && step > lastStep && ok == 0 {
			matched, ok = step, 1
		}
	}
	return matched, ok == 1
}

// TOTPCodeAt returns the code an authenticator app shows at t.
func TOTPCodeAt(secret []byte, t time.Time) string { return totpCode(secret, t.Unix()/totpPeriod) }

// DecodeTOTPSecret parses the base32 form returned by TOTPSecretString.
func DecodeTOTPSecret(s string) ([]byte, error) { return b32.DecodeString(strings.ToUpper(s)) }
