package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"strings"
)

// NewToken returns a 256-bit random token, URL-safe encoded.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how tokens are stored: a leaked database must not yield usable
// session or API tokens. Tokens are high-entropy, so a fast hash is sufficient.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

const RecoveryCodeCount = 10

var recoveryEnc = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

// NewRecoveryCodes returns codes formatted as xxxxx-xxxxx (50 bits each).
func NewRecoveryCodes() []string {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		b := make([]byte, 7)
		rand.Read(b)
		s := recoveryEnc.EncodeToString(b)[:10]
		codes[i] = s[:5] + "-" + s[5:]
	}
	return codes
}

// NormalizeRecoveryCode makes user input comparable with a generated code.
func NormalizeRecoveryCode(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, s)
}

// LooksLikeRecoveryCode distinguishes recovery codes from 6-digit TOTP codes.
func LooksLikeRecoveryCode(s string) bool {
	return len(NormalizeRecoveryCode(s)) == 10
}
