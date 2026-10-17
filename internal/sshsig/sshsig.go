// Package sshsig verifies and makes SSH signatures (ssh-keygen -Y sign,
// PROTOCOL.sshsig): Basalt signs its releases and its catalog with them.
package sshsig

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"

	"golang.org/x/crypto/ssh"
)

// ReleaseKey signs Basalt's releases (namespace ReleaseNamespace) and its
// catalog (another namespace).
const ReleaseKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBUisbbyEb5bDa9jp6Y2foeJ6cqcja04cPmm/BcLGD4v basalt-release"

// ReleaseNamespace is what release checksums are signed for.
const ReleaseNamespace = "basalt-release"

const sigMagic = "SSHSIG"

// sshsig is the signature blob of PROTOCOL.sshsig.
type sshsig struct {
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  string
	HashAlg   string
	Signature []byte
}

// signedData is what the key signs: the magic, namespace, reserved
// field, hash algorithm and the message's hash.
type signedData struct {
	Namespace string
	Reserved  string
	HashAlg   string
	Hash      []byte
}

func messageHash(alg string, msg []byte) ([]byte, error) {
	var h hash.Hash
	switch alg {
	case "sha512":
		h = sha512.New()
	case "sha256":
		h = sha256.New()
	default:
		return nil, fmt.Errorf("unsupported signature hash %q", alg)
	}
	h.Write(msg)
	return h.Sum(nil), nil
}

// Verify checks an armored SSH signature (ssh-keygen -Y sign) of
// msg, made by trusted (an authorized_keys line) for namespace.
func Verify(msg, armored []byte, trusted, namespace string) error {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(trusted))
	if err != nil {
		return fmt.Errorf("invalid trusted key: %w", err)
	}
	block, _ := pem.Decode(armored)
	if block == nil || block.Type != "SSH SIGNATURE" {
		return errors.New("not an SSH signature")
	}
	blob := block.Bytes
	if !bytes.HasPrefix(blob, []byte(sigMagic)) {
		return errors.New("not an SSH signature")
	}
	var sig sshsig
	if err := ssh.Unmarshal(blob[len(sigMagic):], &sig); err != nil {
		return fmt.Errorf("malformed signature: %w", err)
	}
	if sig.Version != 1 {
		return fmt.Errorf("unsupported signature version %d", sig.Version)
	}
	if sig.Namespace != namespace {
		return fmt.Errorf("the signature is for %q, not %q", sig.Namespace, namespace)
	}
	if !bytes.Equal(sig.PublicKey, key.Marshal()) {
		return errors.New("signed by another key than the trusted one")
	}
	var s ssh.Signature
	if err := ssh.Unmarshal(sig.Signature, &s); err != nil {
		return fmt.Errorf("malformed signature: %w", err)
	}
	h, err := messageHash(sig.HashAlg, msg)
	if err != nil {
		return err
	}
	data := append([]byte(sigMagic), ssh.Marshal(signedData{Namespace: sig.Namespace, Reserved: sig.Reserved,
		HashAlg: sig.HashAlg, Hash: h})...)
	if err := key.Verify(data, &s); err != nil {
		return errors.New("the signature does not match")
	}
	return nil
}

// Sign makes an armored SSH signature of msg (as ssh-keygen -Y
// sign does), for tests and tools.
func Sign(msg []byte, signer ssh.Signer, namespace string) ([]byte, error) {
	h, _ := messageHash("sha512", msg)
	data := append([]byte(sigMagic), ssh.Marshal(signedData{Namespace: namespace, HashAlg: "sha512", Hash: h})...)
	s, err := signer.Sign(nil, data)
	if err != nil {
		return nil, err
	}
	blob := append([]byte(sigMagic), ssh.Marshal(sshsig{Version: 1, PublicKey: signer.PublicKey().Marshal(),
		Namespace: namespace, HashAlg: "sha512", Signature: ssh.Marshal(s)})...)
	return pem.EncodeToMemory(&pem.Block{Type: "SSH SIGNATURE", Bytes: blob}), nil
}
