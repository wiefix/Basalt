package webauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

// COSE algorithms accepted (RFC 9053): ES256, EdDSA (Ed25519), RS256.
const (
	AlgES256 = -7
	AlgEdDSA = -8
	AlgRS256 = -257
)

// Algorithms lists them in order of preference, as offered to browsers.
var Algorithms = []int{AlgES256, AlgEdDSA, AlgRS256}

// publicKey is a credential's key decoded from COSE_Key.
type publicKey struct {
	alg int
	ec  *ecdsa.PublicKey
	ed  ed25519.PublicKey
	rsa *rsa.PublicKey
}

func intField(m map[any]any, k int64) (int64, bool) {
	v, ok := m[k].(int64)
	return v, ok
}

func bytesField(m map[any]any, k int64) []byte {
	v, _ := m[k].([]byte)
	return v
}

// parseKey decodes a COSE_Key (RFC 9052) of an accepted algorithm.
func parseKey(cose []byte) (*publicKey, error) {
	v, n, err := decodeCBOR(cose)
	if err != nil {
		return nil, err
	}
	if n != len(cose) {
		return nil, errors.New("trailing data after the public key")
	}
	m, ok := v.(map[any]any)
	if !ok {
		return nil, errors.New("public key is not a COSE key")
	}
	kty, _ := intField(m, 1)
	alg, ok := intField(m, 3)
	if !ok {
		return nil, errors.New("public key without an algorithm")
	}
	switch {
	case kty == 2 && alg == AlgES256:
		crv, _ := intField(m, -1)
		x, y := bytesField(m, -2), bytesField(m, -3)
		if crv != 1 || len(x) != 32 || len(y) != 32 {
			return nil, errors.New("ES256 key is not a P-256 point")
		}
		raw := append(append([]byte{4}, x...), y...)
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			return nil, fmt.Errorf("ES256 key: %w", err)
		}
		return &publicKey{alg: AlgES256, ec: key}, nil
	case kty == 1 && alg == AlgEdDSA:
		crv, _ := intField(m, -1)
		x := bytesField(m, -2)
		if crv != 6 || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("EdDSA key is not an Ed25519 key")
		}
		return &publicKey{alg: AlgEdDSA, ed: ed25519.PublicKey(x)}, nil
	case kty == 3 && alg == AlgRS256:
		n, e := bytesField(m, -1), bytesField(m, -2)
		if len(n) < 256 || len(n) > 512 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("RS256 key must be 2048 to 4096 bits")
		}
		exp := int(new(big.Int).SetBytes(e).Int64())
		if exp < 3 || exp%2 == 0 {
			return nil, errors.New("RS256 key has an invalid exponent")
		}
		return &publicKey{alg: AlgRS256, rsa: &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}}, nil
	}
	return nil, fmt.Errorf("unsupported key (type %d, algorithm %d)", kty, alg)
}

// verify checks sig over data.
func (k *publicKey) verify(data, sig []byte) bool {
	switch k.alg {
	case AlgES256:
		h := sha256.Sum256(data)
		return ecdsa.VerifyASN1(k.ec, h[:], sig)
	case AlgEdDSA:
		return ed25519.Verify(k.ed, data, sig)
	case AlgRS256:
		h := sha256.Sum256(data)
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], sig) == nil
	}
	return false
}
