// Package secrets encrypts sensitive values (TOTP seeds, credentials, env vars)
// before they are written to the database.
//
// Ciphertext layout: version(1) || nonce(24) || XChaCha20-Poly1305(plaintext).
// Callers pass associated data that binds a ciphertext to its owner
// (e.g. "totp:user:42"), so a value copied to another row fails to decrypt.
package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	KeySize = chacha20poly1305.KeySize
	version = 1
)

var ErrDecrypt = errors.New("secrets: decryption failed")

type Box struct {
	key [KeySize]byte
}

func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", KeySize, len(key))
	}
	b := &Box{}
	copy(b.key[:], key)
	return b, nil
}

// LoadOrCreateKey reads the master key at path, generating it on first run.
// The file must be readable by its owner only.
func LoadOrCreateKey(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, KeySize)
		rand.Read(key)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create master key: %w", err)
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, fmt.Errorf("write master key: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("write master key: %w", err)
		}
		return NewBox(key)
	}
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("master key %s has mode %v; must be 0600", path, perm)
	}
	return NewBox(key)
}

func (b *Box) Seal(plaintext, ad []byte) []byte {
	aead, _ := chacha20poly1305.NewX(b.key[:])
	out := make([]byte, 1+aead.NonceSize(), 1+aead.NonceSize()+len(plaintext)+aead.Overhead())
	out[0] = version
	rand.Read(out[1:])
	return aead.Seal(out, out[1:], plaintext, ad)
}

func (b *Box) Open(ciphertext, ad []byte) ([]byte, error) {
	aead, _ := chacha20poly1305.NewX(b.key[:])
	if len(ciphertext) < 1+aead.NonceSize()+aead.Overhead() || ciphertext[0] != version {
		return nil, ErrDecrypt
	}
	nonce := ciphertext[1 : 1+aead.NonceSize()]
	pt, err := aead.Open(nil, nonce, ciphertext[1+aead.NonceSize():], ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// DeriveKey returns a key for another purpose (label), derived from the
// master key: it changes with the master key and reveals nothing about it.
func (b *Box) DeriveKey(label string) []byte {
	mac := hmac.New(sha256.New, b.key[:])
	mac.Write([]byte("basalt-derive:" + label))
	return mac.Sum(nil)
}
