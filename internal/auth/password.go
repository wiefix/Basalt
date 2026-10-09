// Package auth holds the credential primitives: password hashing, TOTP,
// recovery codes, session tokens and rate limiting. It has no knowledge of
// HTTP or the database.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	MinPasswordLen   = 12  // characters
	MaxPasswordBytes = 512 // bounds hashing cost of hostile input
	saltLen          = 16
	keyLen           = 32
)

var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	errMalformedHash    = errors.New("malformed password hash")
)

// ValidatePassword enforces length only, following NIST SP 800-63B:
// composition rules make passwords weaker, not stronger.
func ValidatePassword(pw string) error {
	if len(pw) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	if utf8.RuneCountInString(pw) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	return nil
}

// Argon2Params are the argon2id cost parameters. Memory is in KiB.
type Argon2Params struct {
	Memory  uint32
	Time    uint32
	Threads uint8
}

// DefaultArgon2Params follow RFC 9106 §4 (second recommended option).
var DefaultArgon2Params = Argon2Params{Memory: 64 * 1024, Time: 3, Threads: 2}

// Hasher hashes and verifies passwords. Concurrency is bounded so a burst of
// login attempts cannot exhaust memory (each hash needs Params.Memory KiB).
type Hasher struct {
	params Argon2Params
	sem    chan struct{}
	dummy  string
}

func NewHasher(p Argon2Params, maxConcurrent int) *Hasher {
	h := &Hasher{params: p, sem: make(chan struct{}, max(1, maxConcurrent))}
	h.dummy = h.hash("dummy password for timing equalisation")
	return h
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns a PHC-formatted argon2id hash of pw.
func (h *Hasher) Hash(ctx context.Context, pw string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.hash(pw), nil
}

func (h *Hasher) hash(pw string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	p := h.params
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

// Verify reports whether pw matches encoded. needsRehash is true when the
// hash was made with parameters other than the current ones.
func (h *Hasher) Verify(ctx context.Context, encoded, pw string) (ok, needsRehash bool, err error) {
	if len(pw) > MaxPasswordBytes {
		return false, false, nil
	}
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	ok = subtle.ConstantTimeCompare(got, want) == 1
	return ok, ok && p != h.params, nil
}

// VerifyDummy burns the same time as a real verification. Call it when the
// account does not exist so response timing does not reveal valid emails.
func (h *Hasher) VerifyDummy(ctx context.Context, pw string) {
	_, _, _ = h.Verify(ctx, h.dummy, pw)
}

func parsePHC(s string) (p Argon2Params, salt, key []byte, err error) {
	// $argon2id$v=19$m=65536,t=3,p=2$salt$key
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errMalformedHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return p, nil, nil, errMalformedHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, errMalformedHash
	}
	// Refuse absurd parameters from a tampered database row.
	if p.Memory == 0 || p.Memory > 1<<21 || p.Time == 0 || p.Time > 16 || p.Threads == 0 {
		return p, nil, nil, errMalformedHash
	}
	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return p, nil, nil, errMalformedHash
	}
	if key, err = enc.DecodeString(parts[5]); err != nil || len(key) < 16 {
		return p, nil, nil, errMalformedHash
	}
	return p, salt, key, nil
}
