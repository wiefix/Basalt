package secrets

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// A recovery file holds the master key encrypted with a passphrase the
// owner chose (argon2id, then XChaCha20-Poly1305). It travels with panel
// backups, so a backup can be restored on a new host with the passphrase,
// while a leaked backup without it reveals no secret.

// MinPassphrase is the shortest recovery passphrase accepted.
const MinPassphrase = 12

type recoveryFile struct {
	Format  string `json:"format"` // "basalt-master-key"
	Version int    `json:"version"`
	KDF     string `json:"kdf"` // argon2id
	Memory  uint32 `json:"memory_kib"`
	Time    uint32 `json:"time"`
	Threads uint8  `json:"threads"`
	Salt    []byte `json:"salt"`
	Sealed  []byte `json:"sealed"` // nonce || ciphertext
}

const recoveryAD = "basalt-master-key-v1"

// Bounds on the KDF parameters read from a file, so a crafted file cannot
// exhaust memory.
const (
	maxRecoveryMemory = 1 << 20 // KiB (1 GiB)
	maxRecoveryTime   = 20
)

var ErrRecoveryPassphrase = errors.New("wrong recovery passphrase or damaged recovery file")

func recoveryKey(passphrase string, f *recoveryFile) []byte {
	return argon2.IDKey([]byte(passphrase), f.Salt, f.Time, f.Memory, f.Threads, chacha20poly1305.KeySize)
}

// WrapKey encrypts the box's key with passphrase for a recovery file.
func (b *Box) WrapKey(passphrase string) ([]byte, error) {
	if len(passphrase) < MinPassphrase {
		return nil, fmt.Errorf("the recovery passphrase must have at least %d characters", MinPassphrase)
	}
	f := recoveryFile{Format: "basalt-master-key", Version: 1, KDF: "argon2id", Memory: 64 * 1024, Time: 3, Threads: 2,
		Salt: make([]byte, 16)}
	rand.Read(f.Salt)
	aead, _ := chacha20poly1305.NewX(recoveryKey(passphrase, &f))
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	f.Sealed = aead.Seal(nonce, nonce, b.key[:], []byte(recoveryAD))
	return json.MarshalIndent(f, "", "  ")
}

// UnwrapKey recovers a master key from a recovery file.
func UnwrapKey(data []byte, passphrase string) ([]byte, error) {
	var f recoveryFile
	if err := json.Unmarshal(data, &f); err != nil || f.Format != "basalt-master-key" || f.Version != 1 || f.KDF != "argon2id" {
		return nil, errors.New("not a Basalt recovery file")
	}
	if f.Memory == 0 || f.Memory > maxRecoveryMemory || f.Time == 0 || f.Time > maxRecoveryTime || f.Threads == 0 || len(f.Salt) < 16 {
		return nil, errors.New("unsupported recovery file parameters")
	}
	aead, _ := chacha20poly1305.NewX(recoveryKey(passphrase, &f))
	if len(f.Sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrRecoveryPassphrase
	}
	key, err := aead.Open(nil, f.Sealed[:aead.NonceSize()], f.Sealed[aead.NonceSize():], []byte(recoveryAD))
	if err != nil || len(key) != KeySize {
		return nil, ErrRecoveryPassphrase
	}
	return key, nil
}

// SameKey reports whether key is the box's key.
func (b *Box) SameKey(key []byte) bool { return subtle.ConstantTimeCompare(b.key[:], key) == 1 }
